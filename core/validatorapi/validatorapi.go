// Copyright © 2022-2025 Obol Labs Inc. Licensed under the terms of a Business Source License 1.1

package validatorapi

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"runtime"
	"strconv"
	"testing"
	"time"

	eth2api "github.com/attestantio/go-eth2-client/api"
	eth2v1 "github.com/attestantio/go-eth2-client/api/v1"
	eth2spec "github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/altair"
	eth2p0 "github.com/attestantio/go-eth2-client/spec/phase0"
	ssz "github.com/ferranbt/fastssz"
	"go.opentelemetry.io/otel/trace"

	"github.com/obolnetwork/charon/app/errors"
	"github.com/obolnetwork/charon/app/eth2wrap"
	"github.com/obolnetwork/charon/app/log"
	"github.com/obolnetwork/charon/app/version"
	"github.com/obolnetwork/charon/app/z"
	"github.com/obolnetwork/charon/core"
	"github.com/obolnetwork/charon/eth2util"
	"github.com/obolnetwork/charon/eth2util/eth2exp"
	"github.com/obolnetwork/charon/eth2util/signing"
	"github.com/obolnetwork/charon/tbls"
	"github.com/obolnetwork/charon/tbls/tblsconv"
)

const (
	defaultGasLimit = 30000000
	zeroAddress     = "0x0000000000000000000000000000000000000000"
)

// SlotFromTimestamp returns the Ethereum slot associated to a timestamp, given the genesis configuration fetched
// from client.
func SlotFromTimestamp(ctx context.Context, client eth2wrap.Client, timestamp time.Time) (eth2p0.Slot, error) {
	genesisTime, err := eth2wrap.FetchGenesisTime(ctx, client)
	if err != nil {
		return 0, err
	}

	slotDuration, _, err := eth2wrap.FetchSlotsConfig(ctx, client)
	if err != nil {
		return 0, err
	}

	if timestamp.Before(genesisTime) {
		// if timestamp is in the past (can happen in testing scenarios, there's no strict form of checking on it), fall back on current timestamp.
		nextTimestamp := time.Now()

		log.Info(
			ctx,
			"timestamp before genesis, defaulting to current timestamp",
			z.I64("genesis_timestamp", genesisTime.Unix()),
			z.I64("overridden_timestamp", timestamp.Unix()),
			z.I64("new_timestamp", nextTimestamp.Unix()),
		)

		timestamp = nextTimestamp
	}

	delta := timestamp.Sub(genesisTime)

	return eth2p0.Slot(delta / slotDuration), nil
}

// NewComponentInsecure returns a new instance of the validator API core workflow component
// that does not perform signature verification.
func NewComponentInsecure(_ *testing.T, eth2Cl eth2wrap.Client, shareIdx int) (*Component, error) {
	return &Component{
		eth2Cl:         eth2Cl,
		shareIdx:       shareIdx,
		builderEnabled: false,
		insecureTest:   true,
	}, nil
}

// NewComponent returns a new instance of the validator API core workflow component.
func NewComponent(eth2Cl eth2wrap.Client, allPubSharesByKey map[core.PubKey]map[int]tbls.PublicKey,
	shareIdx int, feeRecipientFunc func(core.PubKey) string, builderEnabled bool, targetGasLimit uint, seenPubkeys func(core.PubKey),
) (*Component, error) {
	var (
		sharesByKey     = make(map[eth2p0.BLSPubKey]eth2p0.BLSPubKey)
		keysByShare     = make(map[eth2p0.BLSPubKey]eth2p0.BLSPubKey)
		sharesByCoreKey = make(map[core.PubKey]tbls.PublicKey)
		coreSharesByKey = make(map[core.PubKey]core.PubKey)
	)

	for corePubkey, shares := range allPubSharesByKey {
		pubshare := shares[shareIdx]

		coreShare, err := core.PubKeyFromBytes(pubshare[:])
		if err != nil {
			return nil, err
		}

		cpBytes, err := corePubkey.Bytes()
		if err != nil {
			return nil, err
		}

		pubkey, err := tblsconv.PubkeyFromBytes(cpBytes)
		if err != nil {
			return nil, err
		}

		eth2Pubkey := eth2p0.BLSPubKey(pubkey)

		eth2Share := eth2p0.BLSPubKey(pubshare)
		sharesByCoreKey[corePubkey] = pubshare
		coreSharesByKey[corePubkey] = coreShare
		sharesByKey[eth2Pubkey] = eth2Share
		keysByShare[eth2Share] = eth2Pubkey
	}

	getVerifyShareFunc := func(pubkey core.PubKey) (tbls.PublicKey, error) {
		pubshare, ok := sharesByCoreKey[pubkey]
		if !ok {
			return tbls.PublicKey{}, errors.New("unknown public key")
		}

		return pubshare, nil
	}

	getPubShareFunc := func(pubkey eth2p0.BLSPubKey) (eth2p0.BLSPubKey, bool) {
		share, ok := sharesByKey[pubkey]

		if seenPubkeys != nil {
			seenPubkeys(core.PubKeyFrom48Bytes(pubkey))
		}

		return share, ok
	}

	getPubKeyFunc := func(share eth2p0.BLSPubKey) (eth2p0.BLSPubKey, error) {
		key, ok := keysByShare[share]
		if !ok {
			for _, shares := range allPubSharesByKey {
				for keyshareIdx, pubshare := range shares {
					if eth2p0.BLSPubKey(pubshare) == share {
						return eth2p0.BLSPubKey{}, errors.New("mismatching validator client key share index, Mth key share submitted to Nth charon peer",
							z.Int("key_share_index", keyshareIdx-1), z.Int("charon_peer_index", shareIdx-1)) // 0-indexed
					}
				}
			}

			return eth2p0.BLSPubKey{}, errors.New("unknown public key")
		}

		if seenPubkeys != nil {
			seenPubkeys(core.PubKeyFrom48Bytes(key))
		}

		return key, nil
	}

	return &Component{
		getVerifyShareFunc: getVerifyShareFunc,
		getPubShareFunc:    getPubShareFunc,
		getPubKeyFunc:      getPubKeyFunc,
		sharesByKey:        coreSharesByKey,
		eth2Cl:             eth2Cl,
		shareIdx:           shareIdx,
		feeRecipientFunc:   feeRecipientFunc,
		builderEnabled:     builderEnabled,
		targetGasLimit:     targetGasLimit,
		swallowRegFilter:   log.Filter(),
	}, nil
}

type Component struct {
	eth2Cl           eth2wrap.Client
	shareIdx         int
	insecureTest     bool
	feeRecipientFunc func(core.PubKey) string
	builderEnabled   bool
	targetGasLimit   uint
	swallowRegFilter z.Field

	// getVerifyShareFunc maps public shares (what the VC thinks as its public key)
	// to public keys (the DV root public key)
	getVerifyShareFunc func(core.PubKey) (tbls.PublicKey, error)
	// getPubShareFunc returns the public share for a root public key.
	getPubShareFunc func(eth2p0.BLSPubKey) (eth2p0.BLSPubKey, bool)
	// getPubKeyFunc returns the root public key for a public share.
	getPubKeyFunc func(eth2p0.BLSPubKey) (eth2p0.BLSPubKey, error)
	// sharesByKey contains this node's public shares (value) by root public (key)
	sharesByKey map[core.PubKey]core.PubKey

	// Registered input functions
	pubKeyByAttFunc           func(ctx context.Context, slot, commIdx, valIdx uint64) (core.PubKey, error)
	awaitAttFunc              func(ctx context.Context, slot, commIdx uint64) (*eth2p0.AttestationData, error)
	awaitProposalFunc         func(ctx context.Context, slot uint64) (*eth2api.VersionedProposal, error)
	awaitSyncContributionFunc func(ctx context.Context, slot, subcommIdx uint64, beaconBlockRoot eth2p0.Root) (*altair.SyncCommitteeContribution, error)
	awaitAggAttFunc           func(ctx context.Context, slot uint64, attestationRoot eth2p0.Root) (*eth2spec.VersionedAttestation, error)
	awaitAggSigDBFunc         func(context.Context, core.Duty, core.PubKey) (core.SignedData, error)
	dutyDefFunc               func(ctx context.Context, duty core.Duty) (core.DutyDefinitionSet, error)
	subs                      []func(context.Context, core.Duty, core.ParSignedDataSet) error
}

// RegisterAwaitProposal registers a function to query unsigned beacon block proposals by providing necessary options.
// It supports a single function, since it is an input of the component.
func (c *Component) RegisterAwaitProposal(fn func(ctx context.Context, slot uint64) (*eth2api.VersionedProposal, error)) {
	c.awaitProposalFunc = fn
}

// RegisterAwaitAttestation registers a function to query attestation data.
// It only supports a single function, since it is an input of the component.
func (c *Component) RegisterAwaitAttestation(fn func(ctx context.Context, slot, commIdx uint64) (*eth2p0.AttestationData, error)) {
	c.awaitAttFunc = fn
}

// RegisterAwaitSyncContribution registers a function to query sync contribution data.
// It only supports a single function, since it is an input of the component.
func (c *Component) RegisterAwaitSyncContribution(fn func(ctx context.Context, slot, subcommIdx uint64, beaconBlockRoot eth2p0.Root) (*altair.SyncCommitteeContribution, error)) {
	c.awaitSyncContributionFunc = fn
}

// RegisterPubKeyByAttestation registers a function to query pubkeys by attestation.
// It only supports a single function, since it is an input of the component.
func (c *Component) RegisterPubKeyByAttestation(fn func(ctx context.Context, slot, commIdx, valIdx uint64) (core.PubKey, error)) {
	c.pubKeyByAttFunc = fn
}

// RegisterGetDutyDefinition registers a function to query duty definitions.
// It supports a single function, since it is an input of the component.
func (c *Component) RegisterGetDutyDefinition(fn func(ctx context.Context, duty core.Duty) (core.DutyDefinitionSet, error)) {
	c.dutyDefFunc = fn
}

// RegisterAwaitAggAttestation registers a function to query an aggregated attestation.
// It supports a single function, since it is an input of the component.
func (c *Component) RegisterAwaitAggAttestation(fn func(ctx context.Context, slot uint64, attestationRoot eth2p0.Root) (*eth2spec.VersionedAttestation, error)) {
	c.awaitAggAttFunc = fn
}

// RegisterAwaitAggSigDB registers a function to query aggregated signed data from aggSigDB.
func (c *Component) RegisterAwaitAggSigDB(fn func(context.Context, core.Duty, core.PubKey) (core.SignedData, error)) {
	c.awaitAggSigDBFunc = fn
}

// Subscribe registers a partial signed data set store function.
// It supports multiple functions since it is the output of the component.
func (c *Component) Subscribe(fn func(context.Context, core.Duty, core.ParSignedDataSet) error) {
	c.subs = append(c.subs, func(ctx context.Context, duty core.Duty, set core.ParSignedDataSet) error {
		// Clone before calling each subscriber.
		clone, err := set.Clone()
		if err != nil {
			return err
		}

		return fn(ctx, duty, clone)
	})
}

// AttestationData implements the eth2client.AttesterDutiesProvider for the router.
func (c Component) AttestationData(ctx context.Context, opts *eth2api.AttestationDataOpts) (*eth2api.Response[*eth2p0.AttestationData], error) {
	att, err := c.awaitAttFunc(ctx, uint64(opts.Slot), uint64(opts.CommitteeIndex))
	if err != nil {
		return nil, err
	}

	return wrapResponse(att), nil
}

// SubmitAttestations implements the eth2client.AttestationsSubmitter for the router.
func (c Component) SubmitAttestations(ctx context.Context, attestationOpts *eth2api.SubmitAttestationsOpts) error {
	attestations := attestationOpts.Attestations
	setsBySlot := make(map[uint64]core.ParSignedDataSet)

	for _, att := range attestations {
		attData, err := att.Data()
		if err != nil {
			return errors.Wrap(err, "get attestation data")
		}

		slot := uint64(attData.Slot)

		attCommitteeIndex, err := att.CommitteeIndex()
		if err != nil {
			return errors.Wrap(err, "get attestation committee index")
		}

		var valIdx eth2p0.ValidatorIndex

		switch att.Version {
		// In pre-electra attestations ValidatorIndex is not part of the VersionedAttestation structure.
		// Try to fetch it by matching the aggregation bits and validator's committee index from the payload to an attester duty from the scheduler.
		case eth2spec.DataVersionPhase0, eth2spec.DataVersionAltair, eth2spec.DataVersionBellatrix, eth2spec.DataVersionCapella, eth2spec.DataVersionDeneb:
			dutyDefSet, err := c.dutyDefFunc(ctx, core.Duty{Slot: uint64(attData.Slot), Type: core.DutyAttester})
			if err != nil {
				return errors.Wrap(err, "duty def set")
			}

			for _, dutyDef := range dutyDefSet {
				attDef, ok := dutyDef.(core.AttesterDefinition)
				if !ok {
					return errors.New("parse duty definition to attester definition")
				}

				if attDef.CommitteeIndex != attData.Index {
					continue
				}

				aggBits, err := att.AggregationBits()
				if err != nil {
					return errors.Wrap(err, "get attestation aggregation bits")
				}

				indices := aggBits.BitIndices()
				if len(indices) != 1 {
					return errors.New("unexpected number of aggregation bits",
						z.Str("aggbits", fmt.Sprintf("%#x", []byte(aggBits))))
				}

				if attDef.ValidatorCommitteeIndex == uint64(indices[0]) {
					valIdx = attDef.ValidatorIndex
					break
				}
			}
		case eth2spec.DataVersionElectra:
			if att.ValidatorIndex == nil {
				return errors.New("missing attestation validator index from electra attestation")
			}

			valIdx = *att.ValidatorIndex
		default:
			return errors.New("invalid attestations version", z.Str("version", att.Version.String()))
		}

		var pubkey core.PubKey

		pubkey, err = c.pubKeyByAttFunc(ctx, slot, uint64(attCommitteeIndex), uint64(valIdx))
		if err != nil {
			return errors.Wrap(err, "failed to find pubkey", z.U64("slot", slot),
				z.U64("commIdx", uint64(attCommitteeIndex)), z.U64("valIdx", uint64(valIdx)))
		}

		parSigData, err := core.NewPartialVersionedAttestation(att, c.shareIdx)
		if err != nil {
			return err
		}

		// Verify attestation signature
		err = c.verifyPartialSig(ctx, parSigData, pubkey)
		if err != nil {
			return err
		}

		// Encode partial signed data and add to a set
		set, ok := setsBySlot[slot]
		if !ok {
			set = make(core.ParSignedDataSet)
			setsBySlot[slot] = set
		}

		set[pubkey] = parSigData
	}

	// Send sets to subscriptions.
	for slot, set := range setsBySlot {
		duty := core.NewAttesterDuty(slot)
		ctx := log.WithCtx(ctx, z.Any("duty", duty))

		for _, sub := range c.subs {
			// No need to clone since sub auto clones.
			err := sub(ctx, duty, set)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (c Component) Proposal(ctx context.Context, opts *eth2api.ProposalOpts) (*eth2api.Response[*eth2api.VersionedProposal], error) {
	// Get proposer pubkey (this is a blocking query).
	pubkey, err := c.getProposerPubkey(ctx, core.NewProposerDuty(uint64(opts.Slot)))
	if err != nil {
		return nil, err
	}

	epoch, err := eth2util.EpochFromSlot(ctx, c.eth2Cl, opts.Slot)
	if err != nil {
		return nil, err
	}

	sigEpoch := eth2util.SignedEpoch{
		Epoch:     epoch,
		Signature: opts.RandaoReveal,
	}

	duty := core.NewRandaoDuty(uint64(opts.Slot))
	parSig := core.NewPartialSignedRandao(sigEpoch.Epoch, sigEpoch.Signature, c.shareIdx)

	// Verify randao signature
	err = c.verifyPartialSig(ctx, parSig, pubkey)
	if err != nil {
		return nil, err
	}

	for _, sub := range c.subs {
		// No need to clone since sub auto clones.
		parsigSet := core.ParSignedDataSet{
			pubkey: parSig,
		}

		err := sub(ctx, duty, parsigSet)
		if err != nil {
			return nil, err
		}
	}

	// In the background, the following needs to happen before the
	// unsigned beacon block will be returned below:
	//  - Threshold number of VCs need to submit their partial randao reveals.
	//  - These signatures will be exchanged and aggregated.
	//  - The aggregated signature will be stored in AggSigDB.
	//  - Scheduler (in the meantime) will schedule a DutyProposer (to create a unsigned block).
	//  - Fetcher will then block waiting for an aggregated randao reveal.
	//  - Once it is found, Fetcher will fetch an unsigned block from the beacon
	//    node including the aggregated randao in the request.
	//  - Consensus will agree upon the unsigned block and insert the resulting block in the DutyDB.
	//  - Once inserted, the query below will return.

	// Query unsigned proposal (this is blocking).
	proposal, err := c.awaitProposalFunc(ctx, uint64(opts.Slot))
	if err != nil {
		return nil, err
	}

	// We do not persist this v3-specific data in the pipeline,
	// but to comply with the API, we need to return non-nil values,
	// and these should be unified across all nodes.
	proposal.ConsensusValue = big.NewInt(1)
	proposal.ExecutionValue = big.NewInt(1)

	return wrapResponse(proposal), nil
}

// propDataMatchesDuty checks that the VC-signed proposal data and prop are the same.
func propDataMatchesDuty(opts *eth2api.SubmitProposalOpts, prop *eth2api.VersionedProposal) error {
	ourPropIdx, err := prop.ProposerIndex()
	if err != nil {
		return errors.Wrap(err, "cannot fetch validator index from dutydb proposal")
	}

	vcPropIdx, err := opts.Proposal.ProposerIndex()
	if err != nil {
		return errors.Wrap(err, "cannot fetch validator index from VC proposal")
	}

	if ourPropIdx != vcPropIdx {
		return errors.New(
			"dutydb and VC proposals have different index",
			z.U64("vc", uint64(vcPropIdx)),
			z.U64("dutydb", uint64(ourPropIdx)),
		)
	}

	if opts.Proposal.Blinded != prop.Blinded {
		return errors.New(
			"dutydb and VC proposals have different blinded value",
			z.Bool("vc", opts.Proposal.Blinded),
			z.Bool("dutydb", prop.Blinded),
		)
	}

	if opts.Proposal.Version != prop.Version {
		return errors.New(
			"dutydb and VC proposals have different version",
			z.Str("vc", opts.Proposal.Version.String()),
			z.Str("dutydb", prop.Version.String()),
		)
	}

	checkHashes := func(d1, d2 ssz.HashRoot) error {
		ddb, err := d1.HashTreeRoot()
		if err != nil {
			return errors.Wrap(err, "hash tree root dutydb")
		}

		if d2 == nil {
			return errors.New("validator client proposal data for the associated dutydb proposal is nil")
		}

		vc, err := d2.HashTreeRoot()
		if err != nil {
			return errors.Wrap(err, "hash tree root dutydb")
		}

		if ddb != vc {
			return errors.New("dutydb and VC proposal data have different hash tree root")
		}

		return nil
	}

	switch prop.Version {
	case eth2spec.DataVersionPhase0:
		return checkHashes(prop.Phase0, opts.Proposal.Phase0.Message)
	case eth2spec.DataVersionAltair:
		return checkHashes(prop.Altair, opts.Proposal.Altair.Message)
	case eth2spec.DataVersionBellatrix:
		if prop.Blinded {
			return checkHashes(prop.BellatrixBlinded, opts.Proposal.BellatrixBlinded.Message)
		}

		return checkHashes(prop.Bellatrix, opts.Proposal.Bellatrix.Message)
	case eth2spec.DataVersionCapella:
		if prop.Blinded {
			return checkHashes(prop.CapellaBlinded, opts.Proposal.CapellaBlinded.Message)
		}

		return checkHashes(prop.Capella, opts.Proposal.Capella.Message)
	case eth2spec.DataVersionDeneb:
		if prop.Blinded {
			return checkHashes(prop.DenebBlinded, opts.Proposal.DenebBlinded.Message)
		}

		return checkHashes(prop.Deneb.Block, opts.Proposal.Deneb.SignedBlock.Message)
	case eth2spec.DataVersionElectra:
		if prop.Blinded {
			return checkHashes(prop.ElectraBlinded, opts.Proposal.ElectraBlinded.Message)
		}

		return checkHashes(prop.Electra.Block, opts.Proposal.Electra.SignedBlock.Message)
	default:
		return errors.New("unexpected block version", z.Str("version", prop.Version.String()))
	}
}

func (c Component) SubmitProposal(ctx context.Context, opts *eth2api.SubmitProposalOpts) error {
	slot, err := opts.Proposal.Slot()
	if err != nil {
		return err
	}

	duty := core.NewProposerDuty(uint64(slot))

	var span trace.Span

	ctx, span = core.StartDutyTrace(ctx, duty, "core/validatorapi.SubmitProposal")
	defer span.End()

	pubkey, err := c.getProposerPubkey(ctx, duty)
	if err != nil {
		return err
	}

	prop, err := c.awaitProposalFunc(ctx, uint64(slot))
	if err != nil {
		return errors.Wrap(err, "could not fetch block definition from dutydb")
	}

	if err := propDataMatchesDuty(opts, prop); err != nil {
		return errors.Wrap(err, "consensus proposal and VC-submitted one do not match")
	}

	// Save Partially Signed Block to ParSigDB
	ctx = log.WithCtx(ctx, z.Any("duty", duty))

	signedData, err := core.NewPartialVersionedSignedProposal(opts.Proposal, c.shareIdx)
	if err != nil {
		return err
	}

	// Verify proposal signature
	err = c.verifyPartialSig(ctx, signedData, pubkey)
	if err != nil {
		return err
	}

	log.Debug(ctx, "Beacon proposal submitted by validator client", z.Str("block_version", opts.Proposal.Version.String()))

	set := core.ParSignedDataSet{pubkey: signedData}
	for _, sub := range c.subs {
		// No need to clone since sub auto clones.
		err = sub(ctx, duty, set)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c Component) SubmitBlindedProposal(ctx context.Context, opts *eth2api.SubmitBlindedProposalOpts) error {
	slot, err := opts.Proposal.Slot()
	if err != nil {
		return err
	}

	duty := core.NewProposerDuty(uint64(slot))

	var span trace.Span

	ctx, span = core.StartDutyTrace(ctx, duty, "core/validatorapi.SubmitBlindedProposal")
	defer span.End()

	ctx = log.WithCtx(ctx, z.Any("duty", duty))

	pubkey, err := c.getProposerPubkey(ctx, duty)
	if err != nil {
		return err
	}

	prop, err := c.awaitProposalFunc(ctx, uint64(slot))
	if err != nil {
		return errors.Wrap(err, "could not fetch block definition from dutydb")
	}

	if err := propDataMatchesDuty(&eth2api.SubmitProposalOpts{
		Common: opts.Common,
		Proposal: &eth2api.VersionedSignedProposal{
			Version:          opts.Proposal.Version,
			Blinded:          true,
			BellatrixBlinded: opts.Proposal.Bellatrix,
			CapellaBlinded:   opts.Proposal.Capella,
			DenebBlinded:     opts.Proposal.Deneb,
			ElectraBlinded:   opts.Proposal.Electra,
		},
		BroadcastValidation: opts.BroadcastValidation,
	}, prop); err != nil {
		return errors.Wrap(err, "consensus proposal and VC-submitted one do not match")
	}

	// Save Partially Signed Blinded Block to ParSigDB
	signedData, err := core.NewPartialVersionedSignedBlindedProposal(opts.Proposal, c.shareIdx)
	if err != nil {
		return err
	}

	// Verify Blinded block signature
	err = c.verifyPartialSig(ctx, signedData, pubkey)
	if err != nil {
		return err
	}

	log.Debug(ctx, "Blinded beacon block submitted by validator client")

	set := core.ParSignedDataSet{pubkey: signedData}
	for _, sub := range c.subs {
		// No need to clone since sub auto clones.
		err = sub(ctx, duty, set)
		if err != nil {
			return err
		}
	}

	return nil
}

// submitRegistration receives the partially signed validator (builder) registration.
func (c Component) submitRegistration(ctx context.Context, registration *eth2api.VersionedSignedValidatorRegistration) error {
	// Note this should be the group pubkey
	eth2Pubkey, err := registration.PubKey()
	if err != nil {
		return err
	}

	pubkey, err := core.PubKeyFromBytes(eth2Pubkey[:])
	if err != nil {
		return err
	}

	if _, ok := c.getPubShareFunc(eth2Pubkey); !ok {
		log.Debug(ctx, "Swallowing non-dv registration, "+
			"this is a known limitation for many validator clients", z.Any("pubkey", pubkey), c.swallowRegFilter)

		return nil
	}

	timestamp, err := registration.Timestamp()
	if err != nil {
		return err
	}

	slot, err := SlotFromTimestamp(ctx, c.eth2Cl, timestamp)
	if err != nil {
		return err
	}

	duty := core.NewBuilderRegistrationDuty(uint64(slot))
	ctx = log.WithCtx(ctx, z.Any("duty", duty))

	signedData, err := core.NewPartialVersionedSignedValidatorRegistration(registration, c.shareIdx)
	if err != nil {
		return err
	}

	// Verify registration signature.
	err = c.verifyPartialSig(ctx, signedData, pubkey)
	if err != nil {
		return err
	}

	// TODO(corver): Batch these for improved network performance
	set := core.ParSignedDataSet{pubkey: signedData}
	for _, sub := range c.subs {
		// No need to clone since sub auto clones.
		err = sub(ctx, duty, set)
		if err != nil {
			return err
		}
	}

	return nil
}

// SubmitValidatorRegistrations receives the partially signed validator (builder) registration.
func (c Component) SubmitValidatorRegistrations(ctx context.Context, registrations []*eth2api.VersionedSignedValidatorRegistration) error {
	if len(registrations) == 0 {
		return nil // Nothing to do
	}

	// Swallow unexpected validator registrations from VCs (for ex: vouch)
	if !c.builderEnabled {
		return nil
	}

	for _, registration := range registrations {
		err := c.submitRegistration(ctx, registration)
		if err != nil {
			return err
		}
	}

	return nil
}

// SubmitVoluntaryExit receives the partially signed voluntary exit.
func (c Component) SubmitVoluntaryExit(ctx context.Context, exit *eth2p0.SignedVoluntaryExit) error {
	vals, err := c.eth2Cl.ActiveValidators(ctx)
	if err != nil {
		return err
	}

	eth2Pubkey, ok := vals[exit.Message.ValidatorIndex]
	if !ok {
		return errors.New("validator not found")
	}

	pubkey, err := core.PubKeyFromBytes(eth2Pubkey[:])
	if err != nil {
		return err
	}

	_, slotsPerEpoch, err := eth2wrap.FetchSlotsConfig(ctx, c.eth2Cl)
	if err != nil {
		return err
	}

	duty := core.NewVoluntaryExit(slotsPerEpoch * uint64(exit.Message.Epoch))
	ctx = log.WithCtx(ctx, z.Any("duty", duty))

	parSigData := core.NewPartialSignedVoluntaryExit(exit, c.shareIdx)

	// Verify voluntary exit signature
	err = c.verifyPartialSig(ctx, parSigData, pubkey)
	if err != nil {
		return err
	}

	log.Info(ctx, "Voluntary exit submitted by validator client")

	for _, sub := range c.subs {
		// No need to clone since sub auto clones.
		err := sub(ctx, duty, core.ParSignedDataSet{pubkey: parSigData})
		if err != nil {
			return err
		}
	}

	return nil
}

// AggregateBeaconCommitteeSelections returns aggregate beacon committee selection proofs.
func (c Component) AggregateBeaconCommitteeSelections(ctx context.Context, selections []*eth2exp.BeaconCommitteeSelection) ([]*eth2exp.BeaconCommitteeSelection, error) {
	vals, err := c.eth2Cl.ActiveValidators(ctx)
	if err != nil {
		return nil, err
	}

	psigsBySlot := make(map[eth2p0.Slot]core.ParSignedDataSet)

	for _, selection := range selections {
		eth2Pubkey, ok := vals[selection.ValidatorIndex]
		if !ok {
			return nil, errors.New("validator not found", z.Any("provided", selection.ValidatorIndex), z.Any("expected", vals.Indices()))
		}

		pubkey, err := core.PubKeyFromBytes(eth2Pubkey[:])
		if err != nil {
			return nil, err
		}

		parSigData := core.NewPartialSignedBeaconCommitteeSelection(selection, c.shareIdx)

		// Verify slot signature.
		err = c.verifyPartialSig(ctx, parSigData, pubkey)
		if err != nil {
			return nil, err
		}

		_, ok = psigsBySlot[selection.Slot]
		if !ok {
			psigsBySlot[selection.Slot] = make(core.ParSignedDataSet)
		}

		psigsBySlot[selection.Slot][pubkey] = parSigData
	}

	for slot, data := range psigsBySlot {
		duty := core.NewPrepareAggregatorDuty(uint64(slot))
		for _, sub := range c.subs {
			err = sub(ctx, duty, data)
			if err != nil {
				return nil, err
			}
		}
	}

	return c.getAggregateBeaconCommSelection(ctx, psigsBySlot)
}

// AggregateAttestation returns the aggregate attestation for the given attestation root.
// It does a blocking query to DutyAggregator unsigned data from dutyDB.
func (c Component) AggregateAttestation(ctx context.Context, opts *eth2api.AggregateAttestationOpts) (*eth2api.Response[*eth2spec.VersionedAttestation], error) {
	aggAtt, err := c.awaitAggAttFunc(ctx, uint64(opts.Slot), opts.AttestationDataRoot)
	if err != nil {
		return nil, err
	}

	return wrapResponse(aggAtt), nil
}

// SubmitAggregateAttestations receives partially signed aggregateAndProofs.
// - It verifies partial signature on AggregateAndProof.
// - It then calls all the subscribers for further steps on partially signed aggregate and proof.
func (c Component) SubmitAggregateAttestations(ctx context.Context, opts *eth2api.SubmitAggregateAttestationsOpts) error {
	aggsAndProofs := opts.SignedAggregateAndProofs

	vals, err := c.eth2Cl.ActiveValidators(ctx)
	if err != nil {
		return err
	}

	psigsBySlot := make(map[eth2p0.Slot]core.ParSignedDataSet)
	for _, agg := range aggsAndProofs {
		slot, err := agg.Slot()
		if err != nil {
			return err
		}

		aggregatorIndex, err := agg.AggregatorIndex()
		if err != nil {
			return err
		}

		eth2Pubkey, ok := vals[aggregatorIndex]
		if !ok {
			return errors.New("validator not found")
		}

		pk, err := core.PubKeyFromBytes(eth2Pubkey[:])
		if err != nil {
			return err
		}

		// Verify inner selection proof (outcome of DutyPrepareAggregator).
		if !c.insecureTest {
			err = signing.VerifyAggregateAndProofSelection(ctx, c.eth2Cl, tbls.PublicKey(eth2Pubkey), agg)
			if err != nil {
				return err
			}
		}

		parSigData := core.NewPartialVersionedSignedAggregateAndProof(agg, c.shareIdx)

		// Verify outer partial signature.
		err = c.verifyPartialSig(ctx, parSigData, pk)
		if err != nil {
			return err
		}

		_, ok = psigsBySlot[slot]
		if !ok {
			psigsBySlot[slot] = make(core.ParSignedDataSet)
		}

		psigsBySlot[slot][pk] = parSigData
	}

	for slot, data := range psigsBySlot {
		duty := core.NewAggregatorDuty(uint64(slot))
		for _, sub := range c.subs {
			err = sub(ctx, duty, data)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// SyncCommitteeContribution returns sync committee contribution data for the given subcommittee and beacon block root.
func (c Component) SyncCommitteeContribution(ctx context.Context, opts *eth2api.SyncCommitteeContributionOpts) (*eth2api.Response[*altair.SyncCommitteeContribution], error) {
	contrib, err := c.awaitSyncContributionFunc(ctx, uint64(opts.Slot), opts.SubcommitteeIndex, opts.BeaconBlockRoot)
	if err != nil {
		return nil, err
	}

	return wrapResponse(contrib), nil
}

// SubmitSyncCommitteeMessages receives the partially signed altair.SyncCommitteeMessage.
func (c Component) SubmitSyncCommitteeMessages(ctx context.Context, messages []*altair.SyncCommitteeMessage) error {
	vals, err := c.eth2Cl.ActiveValidators(ctx)
	if err != nil {
		return err
	}

	psigsBySlot := make(map[eth2p0.Slot]core.ParSignedDataSet)
	for _, msg := range messages {
		slot := msg.Slot

		eth2Pubkey, ok := vals[msg.ValidatorIndex]
		if !ok {
			return errors.New("validator not found")
		}

		pk, err := core.PubKeyFromBytes(eth2Pubkey[:])
		if err != nil {
			return err
		}

		parSigData := core.NewPartialSignedSyncMessage(msg, c.shareIdx)

		err = c.verifyPartialSig(ctx, parSigData, pk)
		if err != nil {
			return err
		}

		_, ok = psigsBySlot[slot]
		if !ok {
			psigsBySlot[slot] = make(core.ParSignedDataSet)
		}

		psigsBySlot[slot][pk] = core.NewPartialSignedSyncMessage(msg, c.shareIdx)
	}

	for slot, data := range psigsBySlot {
		duty := core.NewSyncMessageDuty(uint64(slot))
		for _, sub := range c.subs {
			err = sub(ctx, duty, data)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// SubmitSyncCommitteeContributions receives partially signed altair.SignedContributionAndProof.
// - It verifies partial signature on ContributionAndProof.
// - It then calls all the subscribers for further steps on partially signed contribution and proof.
func (c Component) SubmitSyncCommitteeContributions(ctx context.Context, contributionAndProofs []*altair.SignedContributionAndProof) error {
	vals, err := c.eth2Cl.ActiveValidators(ctx)
	if err != nil {
		return err
	}

	psigsBySlot := make(map[eth2p0.Slot]core.ParSignedDataSet)
	for _, contrib := range contributionAndProofs {
		var (
			slot = contrib.Message.Contribution.Slot
			vIdx = contrib.Message.AggregatorIndex
		)

		eth2Pubkey, ok := vals[vIdx]
		if !ok {
			return errors.New("validator not found")
		}

		pk, err := core.PubKeyFromBytes(eth2Pubkey[:])
		if err != nil {
			return err
		}

		// Verify inner selection proof.
		if !c.insecureTest {
			msg := core.NewSyncContributionAndProof(contrib.Message)

			err = core.VerifyEth2SignedData(ctx, c.eth2Cl, msg, tbls.PublicKey(eth2Pubkey))
			if err != nil {
				return err
			}
		}

		// Verify outer partial signature.
		parSigData := core.NewPartialSignedSyncContributionAndProof(contrib, c.shareIdx)

		err = c.verifyPartialSig(ctx, parSigData, pk)
		if err != nil {
			return err
		}

		_, ok = psigsBySlot[slot]
		if !ok {
			psigsBySlot[slot] = make(core.ParSignedDataSet)
		}

		psigsBySlot[slot][pk] = parSigData
	}

	for slot, data := range psigsBySlot {
		duty := core.NewSyncContributionDuty(uint64(slot))
		for _, sub := range c.subs {
			err = sub(ctx, duty, data)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// AggregateSyncCommitteeSelections returns aggregate sync committee selection proofs.
func (c Component) AggregateSyncCommitteeSelections(ctx context.Context, partialSelections []*eth2exp.SyncCommitteeSelection) ([]*eth2exp.SyncCommitteeSelection, error) {
	vals, err := c.eth2Cl.ActiveValidators(ctx)
	if err != nil {
		return nil, err
	}

	psigsBySlot := make(map[eth2p0.Slot]core.ParSignedDataSet)

	for _, selection := range partialSelections {
		eth2Pubkey, ok := vals[selection.ValidatorIndex]
		if !ok {
			return nil, errors.New("validator not found")
		}

		pubkey, err := core.PubKeyFromBytes(eth2Pubkey[:])
		if err != nil {
			return nil, err
		}

		parSigData := core.NewPartialSignedSyncCommitteeSelection(selection, c.shareIdx)

		// Verify selection proof.
		err = c.verifyPartialSig(ctx, parSigData, pubkey)
		if err != nil {
			return nil, err
		}

		_, ok = psigsBySlot[selection.Slot]
		if !ok {
			psigsBySlot[selection.Slot] = make(core.ParSignedDataSet)
		}

		psigsBySlot[selection.Slot][pubkey] = parSigData
	}

	for slot, data := range psigsBySlot {
		duty := core.NewPrepareSyncContributionDuty(uint64(slot))
		for _, sub := range c.subs {
			err = sub(ctx, duty, data)
			if err != nil {
				return nil, err
			}
		}
	}

	return c.getAggregateSyncCommSelection(ctx, psigsBySlot)
}

// ProposerDuties obtains proposer duties for the given options.
func (c Component) ProposerDuties(ctx context.Context, opts *eth2api.ProposerDutiesOpts) (*eth2api.Response[[]*eth2v1.ProposerDuty], error) {
	eth2Resp, err := c.eth2Cl.ProposerDuties(ctx, opts)
	if err != nil {
		return nil, err
	}

	duties := eth2Resp.Data

	// Replace root public keys with public shares
	for i := range len(duties) {
		if duties[i] == nil {
			return nil, errors.New("proposer duty cannot be nil")
		}

		pubshare, ok := c.getPubShareFunc(duties[i].PubKey)
		if !ok {
			// Ignore unknown validators since ProposerDuties returns ALL proposers for the epoch if validatorIndices is empty.
			continue
		}

		duties[i].PubKey = pubshare
	}

	return wrapResponseWithMetadata(duties, eth2Resp.Metadata), nil
}

func (c Component) AttesterDuties(ctx context.Context, opts *eth2api.AttesterDutiesOpts) (*eth2api.Response[[]*eth2v1.AttesterDuty], error) {
	eth2Resp, err := c.eth2Cl.AttesterDuties(ctx, opts)
	if err != nil {
		return nil, err
	}

	duties := eth2Resp.Data

	// Replace root public keys with public shares.
	for i := range len(duties) {
		if duties[i] == nil {
			return nil, errors.New("attester duty cannot be nil")
		}

		pubshare, ok := c.getPubShareFunc(duties[i].PubKey)
		if !ok {
			return nil, errors.New("pubshare not found")
		}

		duties[i].PubKey = pubshare
	}

	return wrapResponseWithMetadata(duties, eth2Resp.Metadata), nil
}

// SyncCommitteeDuties obtains sync committee duties. If validatorIndices is nil it will return all duties for the given epoch.
func (c Component) SyncCommitteeDuties(ctx context.Context, opts *eth2api.SyncCommitteeDutiesOpts) (*eth2api.Response[[]*eth2v1.SyncCommitteeDuty], error) {
	eth2Resp, err := c.eth2Cl.SyncCommitteeDuties(ctx, opts)
	if err != nil {
		return nil, err
	}

	duties := eth2Resp.Data

	// Replace root public keys with public shares.
	for i := range len(duties) {
		if duties[i] == nil {
			return nil, errors.New("sync committee duty cannot be nil")
		}

		pubshare, ok := c.getPubShareFunc(duties[i].PubKey)
		if !ok {
			return nil, errors.New("pubshare not found")
		}

		duties[i].PubKey = pubshare
	}

	return wrapResponse(duties), nil
}

func (c Component) Validators(ctx context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error) {
	if len(opts.PubKeys) == 0 && len(opts.Indices) == 0 {
		// fetch all validators
		eth2Resp, err := c.eth2Cl.Validators(ctx, opts)
		if err != nil {
			return nil, err
		}

		convertedVals, err := c.convertValidators(eth2Resp.Data, len(opts.Indices) == 0)
		if err != nil {
			return nil, err
		}

		return wrapResponse(convertedVals), nil
	}

	cachedValidators, err := c.eth2Cl.CompleteValidators(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "can't fetch complete validators cache")
	}

	// Match pubshares to the associated full validator public key
	var pubkeys []eth2p0.BLSPubKey

	for _, pubshare := range opts.PubKeys {
		pubkey, err := c.getPubKeyFunc(pubshare)
		if err != nil {
			return nil, err
		}

		pubkeys = append(pubkeys, pubkey)
	}

	var (
		nonCachedPubkeys []eth2p0.BLSPubKey
		ret              = make(map[eth2p0.ValidatorIndex]*eth2v1.Validator)
	)

	// Index cached validators by their pubkey for quicker lookup
	cvMap := make(map[eth2p0.BLSPubKey]eth2p0.ValidatorIndex)
	for vIdx, cpubkey := range cachedValidators {
		cvMap[cpubkey.Validator.PublicKey] = vIdx
	}

	// Check if any of the pubkeys passed as argument are already cached
	for _, ncVal := range pubkeys {
		vIdx, ok := cvMap[ncVal]
		if !ok {
			nonCachedPubkeys = append(nonCachedPubkeys, ncVal)
			continue
		}

		ret[vIdx] = cachedValidators[vIdx]
	}

	if len(nonCachedPubkeys) != 0 || len(opts.Indices) > 0 {
		log.Debug(ctx, "Requesting validators to upstream beacon node", z.Int("non_cached_pubkeys_amount", len(nonCachedPubkeys)), z.Int("indices", len(opts.Indices)))

		opts.PubKeys = nonCachedPubkeys

		eth2Resp, err := c.eth2Cl.Validators(ctx, opts)
		if err != nil {
			return nil, errors.Wrap(err, "fetching non-cached validators from BN")
		}

		maps.Copy(ret, eth2Resp.Data)
	} else {
		log.Debug(ctx, "All validators requested were cached", z.Int("amount_requested", len(opts.PubKeys)))
	}

	convertedVals, err := c.convertValidators(ret, len(opts.Indices) == 0)
	if err != nil {
		return nil, err
	}

	return wrapResponse(convertedVals), nil
}

// NodeVersion returns the current version of charon.
func (Component) NodeVersion(context.Context, *eth2api.NodeVersionOpts) (*eth2api.Response[string], error) {
	commitSHA, _ := version.GitCommit()
	charonVersion := fmt.Sprintf("obolnetwork/charon/%v-%s/%s-%s", version.Version, commitSHA, runtime.GOARCH, runtime.GOOS)

	return wrapResponse(charonVersion), nil
}

// convertValidators returns the validator map with root public keys replaced by public shares for all validators that are part of the cluster.
func (c Component) convertValidators(vals map[eth2p0.ValidatorIndex]*eth2v1.Validator, ignoreNotFound bool) (map[eth2p0.ValidatorIndex]*eth2v1.Validator, error) {
	resp := make(map[eth2p0.ValidatorIndex]*eth2v1.Validator)
	for vIdx, rawVal := range vals {
		if rawVal == nil || rawVal.Validator == nil {
			return nil, errors.New("validator data cannot be nil")
		}

		innerVal := *rawVal.Validator

		pubshare, ok := c.getPubShareFunc(innerVal.PublicKey)
		if !ok && !ignoreNotFound {
			return nil, errors.New("pubshare not found")
		} else if ok {
			innerVal.PublicKey = pubshare
		}

		var val eth2v1.Validator

		val.Index = rawVal.Index
		val.Status = rawVal.Status
		val.Balance = rawVal.Balance
		val.Validator = &innerVal

		resp[vIdx] = &val
	}

	return resp, nil
}

func (c Component) getProposerPubkey(ctx context.Context, duty core.Duty) (core.PubKey, error) {
	// Get proposer pubkey (this is a blocking query).
	defSet, err := c.dutyDefFunc(ctx, duty)
	if err != nil {
		return "", err
	} else if len(defSet) != 1 {
		return "", errors.New("unexpected amount of proposer duties")
	}

	// There should be single duty proposer for the slot
	var pubkey core.PubKey
	for pk := range defSet {
		pubkey = pk
	}

	return pubkey, nil
}

func (c Component) verifyPartialSig(ctx context.Context, parSig core.ParSignedData, pubkey core.PubKey) error {
	if c.insecureTest {
		return nil
	}

	pubshare, err := c.getVerifyShareFunc(pubkey)
	if err != nil {
		return err
	}

	eth2Signed, ok := parSig.SignedData.(core.Eth2SignedData)
	if !ok {
		return errors.New("invalid eth2 signed data")
	}

	return core.VerifyEth2SignedData(ctx, c.eth2Cl, eth2Signed, pubshare)
}

func (c Component) getAggregateBeaconCommSelection(ctx context.Context, psigsBySlot map[eth2p0.Slot]core.ParSignedDataSet) ([]*eth2exp.BeaconCommitteeSelection, error) {
	var resp []*eth2exp.BeaconCommitteeSelection

	for slot, data := range psigsBySlot {
		duty := core.NewPrepareAggregatorDuty(uint64(slot))
		for pk := range data {
			// Query aggregated subscription from aggsigdb for each duty and public key (this is blocking).
			s, err := c.awaitAggSigDBFunc(ctx, duty, pk)
			if err != nil {
				return nil, err
			}

			sub, ok := s.(core.BeaconCommitteeSelection)
			if !ok {
				return nil, errors.New("invalid beacon committee selection")
			}

			resp = append(resp, &sub.BeaconCommitteeSelection)
		}
	}

	return resp, nil
}

func (c Component) getAggregateSyncCommSelection(ctx context.Context, psigsBySlot map[eth2p0.Slot]core.ParSignedDataSet) ([]*eth2exp.SyncCommitteeSelection, error) {
	var resp []*eth2exp.SyncCommitteeSelection

	for slot, data := range psigsBySlot {
		duty := core.NewPrepareSyncContributionDuty(uint64(slot))
		for pk := range data {
			// Query aggregated sync committee selection from aggsigdb for each duty and public key (this is blocking).
			s, err := c.awaitAggSigDBFunc(ctx, duty, pk)
			if err != nil {
				return nil, err
			}

			sub, ok := s.(core.SyncCommitteeSelection)
			if !ok {
				return nil, errors.New("invalid sync committee selection")
			}

			resp = append(resp, &sub.SyncCommitteeSelection)
		}
	}

	return resp, nil
}

// ProposerConfig returns the proposer configuration for all validators.
func (c Component) ProposerConfig(ctx context.Context) (*eth2exp.ProposerConfigResponse, error) {
	var targetGasLimit uint
	if c.targetGasLimit == 0 {
		log.Warn(ctx, "", errors.New("custom target gas limit not supported, setting to default", z.Uint("default_gas_limit", defaultGasLimit)))
		targetGasLimit = defaultGasLimit
	} else {
		targetGasLimit = c.targetGasLimit
	}

	resp := eth2exp.ProposerConfigResponse{
		Proposers: make(map[eth2p0.BLSPubKey]eth2exp.ProposerConfig),
		Default: eth2exp.ProposerConfig{ // Default doesn't make sense, disable for now.
			FeeRecipient: zeroAddress,
			Builder: eth2exp.Builder{
				Enabled:  false,
				GasLimit: targetGasLimit,
			},
		},
	}

	genesisTime, err := eth2wrap.FetchGenesisTime(ctx, c.eth2Cl)
	if err != nil {
		return nil, err
	}

	slotDuration, _, err := eth2wrap.FetchSlotsConfig(ctx, c.eth2Cl)
	if err != nil {
		return nil, err
	}

	timestamp := genesisTime
	timestamp = timestamp.Add(slotDuration) // Use slot 1 for timestamp to override pre-generated registrations.

	for pubkey, pubshare := range c.sharesByKey {
		eth2Share, err := pubshare.ToETH2()
		if err != nil {
			return nil, err
		}

		resp.Proposers[eth2Share] = eth2exp.ProposerConfig{
			FeeRecipient: c.feeRecipientFunc(pubkey),
			Builder: eth2exp.Builder{
				Enabled:  c.builderEnabled,
				GasLimit: targetGasLimit,
				Overrides: map[string]string{
					"timestamp":  strconv.FormatInt(timestamp.Unix(), 10),
					"public_key": string(pubkey),
				},
			},
		}
	}

	return &resp, nil
}

// wrapResponse wraps the provided data into an API Response and returns the response.
func wrapResponse[T any](data T) *eth2api.Response[T] {
	return &eth2api.Response[T]{Data: data}
}

// wrapResponseWithMetadata wraps the provided data and metadata into an API Response and returns the response.
func wrapResponseWithMetadata[T any](data T, metadata map[string]any) *eth2api.Response[T] {
	return &eth2api.Response[T]{Data: data, Metadata: metadata}
}
