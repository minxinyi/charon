// Copyright © 2022-2025 Obol Labs Inc. Licensed under the terms of a Business Source License 1.1

package validatorapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	eth2client "github.com/attestantio/go-eth2-client"
	eth2api "github.com/attestantio/go-eth2-client/api"
	eth2v1 "github.com/attestantio/go-eth2-client/api/v1"
	eth2bellatrix "github.com/attestantio/go-eth2-client/api/v1/bellatrix"
	eth2capella "github.com/attestantio/go-eth2-client/api/v1/capella"
	eth2deneb "github.com/attestantio/go-eth2-client/api/v1/deneb"
	eth2electra "github.com/attestantio/go-eth2-client/api/v1/electra"
	eth2http "github.com/attestantio/go-eth2-client/http"
	eth2mock "github.com/attestantio/go-eth2-client/mock"
	eth2spec "github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/altair"
	"github.com/attestantio/go-eth2-client/spec/bellatrix"
	"github.com/attestantio/go-eth2-client/spec/capella"
	"github.com/attestantio/go-eth2-client/spec/electra"
	eth2p0 "github.com/attestantio/go-eth2-client/spec/phase0"
	ssz "github.com/ferranbt/fastssz"
	"github.com/stretchr/testify/require"

	"github.com/obolnetwork/charon/app/errors"
	"github.com/obolnetwork/charon/app/eth2wrap"
	"github.com/obolnetwork/charon/eth2util/eth2exp"
	"github.com/obolnetwork/charon/testutil"
)

const (
	slotsPerEpoch    = 32
	electraForkEpoch = 1
	infoLevel        = 1 // 1 is InfoLevel, this avoids importing zerolog directly.
)

type addr string

func (a addr) Address() string {
	return string(a)
}

func TestProxyShutdown(t *testing.T) {
	// Start a server that will block until the request is cancelled.
	serving := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(serving)
		<-r.Context().Done()
	}))

	// Start a proxy server that will proxy to the target server.
	ctx, cancel := context.WithCancel(context.Background())
	proxy := httptest.NewServer(proxyHandler(ctx, addr(target.URL)))

	// Make a request to the proxy server, this will block until the proxy is shutdown.
	errCh := make(chan error, 1)

	go func() {
		_, err := http.Get(proxy.URL)
		errCh <- err
	}()

	// Wait for the target server is serving the request.
	<-serving
	// Shutdown the proxy server.
	cancel()
	// Wait for the request to complete.
	err := <-errCh
	require.NoError(t, err)
}

func TestRouterIntegration(t *testing.T) {
	beaconURL, ok := os.LookupEnv("BEACON_URL")
	if !ok {
		t.Skip("Skipping integration test since BEACON_URL not found")
	}

	r, err := NewRouter(context.Background(), Handler(nil), testBeaconAddr{addr: beaconURL}, true)
	require.NoError(t, err)

	server := httptest.NewServer(r)
	defer server.Close()

	resp, err := http.Get(server.URL + "/eth/v1/node/version")
	require.NoError(t, err)

	require.Equal(t, 200, resp.StatusCode)
}

func TestRawRouter(t *testing.T) {
	t.Run("proxy", func(t *testing.T) {
		handler := testHandler{
			ProxyHandler: func(w http.ResponseWriter, r *http.Request) {
				b, err := httputil.DumpRequest(r, false)
				require.NoError(t, err)
				_, _ = w.Write(b)
			},
		}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Get(baseURL + "/foo?bar=123")
			require.NoError(t, err)
			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)
			require.Contains(t, string(body), "GET /foo?bar=123")
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("invalid path param", func(t *testing.T) {
		handler := testHandler{}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Post(baseURL+"/eth/v1/validator/duties/attester/not_a_number", "application/json", bytes.NewReader([]byte("{}")))
			require.NoError(t, err)

			var errRes errorResponse

			err = json.NewDecoder(res.Body).Decode(&errRes)
			require.NoError(t, err)
			require.Equal(t, errRes, errorResponse{
				Code:    http.StatusBadRequest,
				Message: "invalid uint path parameter epoch [not_a_number]",
			})
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("missing query params", func(t *testing.T) {
		handler := testHandler{}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Get(baseURL + "/eth/v3/validator/blocks/123")
			require.NoError(t, err)

			var errRes errorResponse

			err = json.NewDecoder(res.Body).Decode(&errRes)
			require.NoError(t, err)
			require.Equal(t, errRes, errorResponse{
				Code:    http.StatusBadRequest,
				Message: "missing 0x-hex query parameter randao_reveal",
			})
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("invalid length query params", func(t *testing.T) {
		handler := testHandler{}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Get(baseURL + "/eth/v3/validator/blocks/123?randao_reveal=0x0000")
			require.NoError(t, err)

			var errRes errorResponse

			err = json.NewDecoder(res.Body).Decode(&errRes)
			require.NoError(t, err)
			require.Equal(t, errRes, errorResponse{
				Code:    http.StatusBadRequest,
				Message: "invalid length for 0x-hex query parameter randao_reveal, expect 96 bytes",
			})
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("empty graffiti", func(t *testing.T) {
		handler := testHandler{}
		handler.ProposalFunc = func(ctx context.Context, opts *eth2api.ProposalOpts) (*eth2api.Response[*eth2api.VersionedProposal], error) {
			require.Empty(t, opts.Graffiti)

			resp := testutil.RandomDenebVersionedProposal()

			return wrapResponse(resp), nil
		}

		callback := func(ctx context.Context, baseURL string) {
			randao := testutil.RandomEth2Signature().String()
			res, err := http.Get(baseURL + "/eth/v3/validator/blocks/123?randao_reveal=" + randao)
			require.NoError(t, err)

			var okResp struct{ Data json.RawMessage }

			err = json.NewDecoder(res.Body).Decode(&okResp)
			require.NoError(t, err)
			require.NotEmpty(t, okResp.Data)
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("empty body", func(t *testing.T) {
		handler := testHandler{}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Post(baseURL+"/eth/v1/validator/duties/attester/1", "application/json", bytes.NewReader([]byte("")))
			require.NoError(t, err)

			var errRes errorResponse

			err = json.NewDecoder(res.Body).Decode(&errRes)
			require.NoError(t, err)
			require.Equal(t, errRes, errorResponse{
				Code:    http.StatusBadRequest,
				Message: "empty request body",
			})
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("invalid request body", func(t *testing.T) {
		handler := testHandler{}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Post(baseURL+"/eth/v1/validator/duties/attester/1", "", strings.NewReader("not json"))
			require.NoError(t, err)

			var errRes errorResponse

			err = json.NewDecoder(res.Body).Decode(&errRes)
			require.NoError(t, err)
			require.Equal(t, errRes, errorResponse{
				Code:    http.StatusBadRequest,
				Message: "failed parsing json request body",
			})
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("valid content type in 2xx response", func(t *testing.T) {
		handler := testHandler{}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Get(baseURL + "/eth/v1/node/version")
			require.NoError(t, err)
			require.Equal(t, res.Header.Get("Content-Type"), "application/json")
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("valid content type in non-2xx response", func(t *testing.T) {
		handler := testHandler{}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Post(baseURL+"/eth/v1/validator/duties/attester/1", "", strings.NewReader("not json"))
			require.NoError(t, err)
			require.Equal(t, res.Header.Get("Content-Type"), "application/json")

			var errRes errorResponse
			require.NoError(t, json.NewDecoder(res.Body).Decode(&errRes))
			require.Equal(t, errRes.Code, http.StatusBadRequest)
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("client timeout", func(t *testing.T) {
		cctx, cancel := context.WithCancel(context.Background())
		handler := testHandler{
			ValidatorsFunc: func(sctx context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error) {
				cancel()      // Ensure that cancelling client context (cctx)
				<-sctx.Done() // Results in server context (sctx) being closed.

				return nil, sctx.Err()
			},
		}

		callback := func(_ context.Context, baseURL string) {
			req, err := http.NewRequestWithContext(cctx, http.MethodGet, baseURL+"/eth/v1/beacon/states/head/validators/12", nil)
			require.NoError(t, err)

			_, err = new(http.Client).Do(req)
			if !errors.Is(err, context.Canceled) {
				require.NoError(t, err)
			}
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("get_single_validators", func(t *testing.T) {
		handler := testHandler{
			ValidatorsFunc: func(_ context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error) {
				res := make(map[eth2p0.ValidatorIndex]*eth2v1.Validator)
				for _, index := range opts.Indices {
					res[index] = &eth2v1.Validator{
						Index:  index,
						Status: eth2v1.ValidatorStateActiveOngoing,
						Validator: &eth2p0.Validator{
							PublicKey:             testutil.RandomEth2PubKey(t),
							WithdrawalCredentials: []byte("12345678901234567890123456789012"),
						},
					}
				}

				return wrapResponse(res), nil
			},
		}

		callback := func(ctx context.Context, baseURL string) {
			res, err := http.Get(baseURL + "/eth/v1/beacon/states/head/validators/12")
			require.NoError(t, err)

			resp := struct {
				Data *eth2v1.Validator `json:"data"`
			}{}
			err = json.NewDecoder(res.Body).Decode(&resp)
			require.NoError(t, err)
			require.EqualValues(t, 12, resp.Data.Index)
		}

		testRawRouter(t, handler, callback)
	})

	t.Run("get validators with post", func(t *testing.T) {
		simpleValidatorsFunc := func(_ context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error) { //nolint:unparam
			res := make(map[eth2p0.ValidatorIndex]*eth2v1.Validator)
			if len(opts.Indices) == 0 {
				opts.Indices = []eth2p0.ValidatorIndex{12, 35}
			}

			for _, index := range opts.Indices {
				res[index] = &eth2v1.Validator{
					Index:  index,
					Status: eth2v1.ValidatorStateActiveOngoing,
					Validator: &eth2p0.Validator{
						PublicKey:             testutil.RandomEth2PubKey(t),
						WithdrawalCredentials: []byte("12345678901234567890123456789012"),
					},
				}
			}

			return wrapResponse(res), nil
		}

		assertResults := func(t *testing.T, expected []uint64, res *http.Response) {
			t.Helper()

			resp := struct {
				Data []*eth2v1.Validator `json:"data"`
			}{}
			err := json.NewDecoder(res.Body).Decode(&resp)
			require.NoError(t, err)
			require.Len(t, resp.Data, 2)

			var indices []uint64
			for _, vr := range resp.Data {
				indices = append(indices, uint64(vr.Index))
			}

			require.ElementsMatch(t, expected, indices)
		}

		uintToStrArr := func(t *testing.T, data []uint64) []string {
			t.Helper()

			ret := make([]string, 0, len(data))

			for _, d := range data {
				ret = append(ret, strconv.FormatUint(d, 10))
			}

			return ret
		}

		t.Run("via query ids", func(t *testing.T) {
			handler := testHandler{ValidatorsFunc: simpleValidatorsFunc}

			values := []uint64{12, 35}

			callback := func(ctx context.Context, baseURL string) {
				res, err := http.Post(baseURL+"/eth/v1/beacon/states/head/validators?id="+strings.Join(uintToStrArr(t, values), ","), "application/json", bytes.NewReader([]byte{}))
				require.NoError(t, err)
				assertResults(t, values, res)
			}

			testRawRouter(t, handler, callback)
		})

		t.Run("via post body", func(t *testing.T) {
			handler := testHandler{ValidatorsFunc: simpleValidatorsFunc}

			values := []uint64{12, 35}

			callback := func(ctx context.Context, baseURL string) {
				b := struct {
					IDs []string `json:"ids"`
				}{
					IDs: uintToStrArr(t, values),
				}

				bb, err := json.Marshal(b)
				require.NoError(t, err)

				res, err := http.Post(baseURL+"/eth/v1/beacon/states/head/validators", "application/json", bytes.NewReader(bb))
				require.NoError(t, err)
				assertResults(t, values, res)
			}

			testRawRouter(t, handler, callback)
		})

		t.Run("empty parameters", func(t *testing.T) {
			handler := testHandler{ValidatorsFunc: simpleValidatorsFunc}

			callback := func(ctx context.Context, baseURL string) {
				res, err := http.Post(baseURL+"/eth/v1/beacon/states/head/validators", "application/json", bytes.NewReader([]byte{}))
				require.NoError(t, err)

				// when no validator ids are specified, this function will return always the complete
				// list of validators
				assertResults(t, []uint64{12, 35}, res)
			}

			testRawRouter(t, handler, callback)
		})

		t.Run("propose_block returns 404", func(t *testing.T) {
			handler := testHandler{}

			callback := func(ctx context.Context, baseURL string) {
				res, err := http.Post(baseURL+"/eth/v2/validator/blocks/123", "application/json", bytes.NewReader([]byte{}))
				require.NoError(t, err)
				require.Equal(t, http.StatusNotFound, res.StatusCode)
			}

			testRawRouter(t, handler, callback)
		})

		t.Run("propose_blinded_block returns 404", func(t *testing.T) {
			handler := testHandler{}

			callback := func(ctx context.Context, baseURL string) {
				res, err := http.Post(baseURL+"/eth/v1/validator/blinded_blocks/123", "application/json", bytes.NewReader([]byte{}))
				require.NoError(t, err)
				require.Equal(t, http.StatusNotFound, res.StatusCode)
			}

			testRawRouter(t, handler, callback)
		})
	})

	t.Run("submit bellatrix ssz proposal", func(t *testing.T) {
		var done atomic.Bool

		coreBlock := testutil.RandomBellatrixCoreVersionedSignedProposal()
		proposal := &coreBlock.VersionedSignedProposal

		handler := testHandler{
			SubmitProposalFunc: func(ctx context.Context, actual *eth2api.SubmitProposalOpts) error {
				require.Equal(t, proposal, actual.Proposal)
				done.Store(true)

				return nil
			},
		}

		callback := func(ctx context.Context, baseURL string) {
			b, err := ssz.MarshalSSZ(proposal.Bellatrix)
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				baseURL+"/eth/v1/beacon/blocks", bytes.NewReader(b))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")

			resp, err := new(http.Client).Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}

		testRawRouter(t, handler, callback)
		require.True(t, done.Load())
	})

	t.Run("submit capella ssz beacon block", func(t *testing.T) {
		var done atomic.Bool

		coreBlock := testutil.RandomCapellaCoreVersionedSignedProposal()
		proposal := &coreBlock.VersionedSignedProposal

		handler := testHandler{
			SubmitProposalFunc: func(ctx context.Context, actual *eth2api.SubmitProposalOpts) error {
				require.Equal(t, proposal, actual.Proposal)
				done.Store(true)

				return nil
			},
		}

		callback := func(ctx context.Context, baseURL string) {
			b, err := ssz.MarshalSSZ(proposal.Capella)
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				baseURL+"/eth/v1/beacon/blocks", bytes.NewReader(b))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")

			resp, err := new(http.Client).Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}

		testRawRouter(t, handler, callback)
		require.True(t, done.Load())
	})

	t.Run("submit deneb ssz beacon block", func(t *testing.T) {
		var done atomic.Bool

		coreBlock := testutil.RandomDenebCoreVersionedSignedProposal()
		proposal := &coreBlock.VersionedSignedProposal

		handler := testHandler{
			SubmitProposalFunc: func(ctx context.Context, actual *eth2api.SubmitProposalOpts) error {
				require.Equal(t, proposal, actual.Proposal)
				done.Store(true)

				return nil
			},
		}

		callback := func(ctx context.Context, baseURL string) {
			b, err := ssz.MarshalSSZ(proposal.Deneb)
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				baseURL+"/eth/v2/beacon/blocks", bytes.NewReader(b))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")

			resp, err := new(http.Client).Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}

		testRawRouter(t, handler, callback)
		require.True(t, done.Load())
	})

	t.Run("submit electra ssz beacon block", func(t *testing.T) {
		var done atomic.Bool

		coreBlock := testutil.RandomElectraCoreVersionedSignedProposal()
		proposal := &coreBlock.VersionedSignedProposal

		handler := testHandler{
			SubmitProposalFunc: func(ctx context.Context, actual *eth2api.SubmitProposalOpts) error {
				require.Equal(t, proposal, actual.Proposal)
				done.Store(true)

				return nil
			},
		}

		callback := func(ctx context.Context, baseURL string) {
			b, err := ssz.MarshalSSZ(proposal.Electra)
			require.NoError(t, err)

			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				baseURL+"/eth/v2/beacon/blocks", bytes.NewReader(b))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/octet-stream")

			resp, err := new(http.Client).Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}

		testRawRouter(t, handler, callback)
		require.True(t, done.Load())
	})

	t.Run("get response header for block proposal v3", func(t *testing.T) {
		block := &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionCapella,
			Capella:        testutil.RandomCapellaBeaconBlock(),
			ExecutionValue: big.NewInt(123),
			ConsensusValue: big.NewInt(456),
		}
		expectedSlot, err := block.Slot()
		require.NoError(t, err)

		randao := block.Capella.Body.RANDAOReveal

		handler := testHandler{
			ProposalFunc: func(ctx context.Context, opts *eth2api.ProposalOpts) (*eth2api.Response[*eth2api.VersionedProposal], error) {
				require.Equal(t, expectedSlot, opts.Slot)
				require.Equal(t, randao, opts.RandaoReveal)

				return wrapResponse(block), nil
			},
		}

		mustGetRequest := func(baseURL string, expectedSlot eth2p0.Slot, expectedRandao eth2p0.BLSSignature) *http.Response {
			res, err := http.Get(baseURL + fmt.Sprintf("/eth/v3/validator/blocks/%d?randao_reveal=%#x", expectedSlot, expectedRandao))
			require.NoError(t, err)

			return res
		}

		callback := func(ctx context.Context, baseURL string) {
			res := mustGetRequest(baseURL, expectedSlot, randao)

			// Verify response header.
			require.Equal(t, block.Version.String(), res.Header.Get(versionHeader))
			require.Equal(t, "false", res.Header.Get(executionPayloadBlindedHeader))
			require.Equal(t, block.ExecutionValue.String(), res.Header.Get(executionPayloadValueHeader))
			require.Equal(t, block.ConsensusValue.String(), res.Header.Get(consensusBlockValueHeader))

			var blockRes proposeBlockV3Response

			err = json.NewDecoder(res.Body).Decode(&blockRes)
			require.NoError(t, err)
			require.Equal(t, block.Blinded, blockRes.ExecutionPayloadBlinded)
			require.Equal(t, block.ExecutionValue.String(), blockRes.ExecutionPayloadValue)
			require.Equal(t, block.ConsensusValue.String(), blockRes.ConsensusBlockValue)
		}

		// BuilderAPI is disabled, we expect to get the blinded block
		testRawRouterEx(t, handler, callback, true)
	})
}

//nolint:maintidx // This function is a test of tests, so analysed as "complex".
func TestRouter(t *testing.T) {
	var dependentRoot eth2p0.Root

	_, _ = rand.Read(dependentRoot[:])

	metadata := map[string]any{
		"execution_optimistic": true,
		"dependent_root":       dependentRoot,
	}

	t.Run("wrong http method", func(t *testing.T) {
		ctx := context.Background()

		h := testHandler{}

		proxy := httptest.NewServer(h.newBeaconHandler(t))
		defer proxy.Close()

		r, err := NewRouter(ctx, h, testBeaconAddr{addr: proxy.URL}, true)
		require.NoError(t, err)

		server := httptest.NewServer(r)
		defer server.Close()

		endpointURL := server.URL + "/eth/v1/node/version"

		// node_version is a GET-only endpoint, we expect it to fail
		resp, err := http.Post(
			endpointURL,
			"application/json",
			bytes.NewReader([]byte("{}")),
		)

		require.NoError(t, err)

		require.Equal(
			t,
			http.StatusNotFound,
			resp.StatusCode,
		)

		// use the right http method and expect a response, and status code 200
		resp, err = http.Get(endpointURL)
		require.NoError(t, err)

		require.Equal(
			t,
			http.StatusOK,
			resp.StatusCode,
		)

		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		defer func() {
			_ = resp.Body.Close()
		}()

		require.NotEmpty(t, data)
	})

	t.Run("attesterduty", func(t *testing.T) {
		handler := testHandler{
			AttesterDutiesFunc: func(ctx context.Context, opts *eth2api.AttesterDutiesOpts) (*eth2api.Response[[]*eth2v1.AttesterDuty], error) {
				var res []*eth2v1.AttesterDuty
				for _, index := range opts.Indices {
					res = append(res, &eth2v1.AttesterDuty{
						ValidatorIndex:   index,                                   // Echo index
						Slot:             eth2p0.Slot(slotsPerEpoch * opts.Epoch), // Echo first slot of epoch
						CommitteeLength:  1,                                       // 0 fails validation
						CommitteesAtSlot: 1,                                       // 0 fails validation
					})
				}

				return wrapResponseWithMetadata(res, metadata), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			const (
				slotEpoch = 1
				index0    = 2
				index1    = 3
			)

			opts := &eth2api.AttesterDutiesOpts{
				Epoch: eth2p0.Epoch(slotEpoch),
				Indices: []eth2p0.ValidatorIndex{
					eth2p0.ValidatorIndex(index0),
					eth2p0.ValidatorIndex(index1),
				},
			}
			resp, err := cl.AttesterDuties(ctx, opts)
			require.NoError(t, err)

			res := resp.Data

			require.Len(t, res, 2)
			require.Equal(t, int(res[0].Slot), slotEpoch*slotsPerEpoch)
			require.Equal(t, int(res[0].ValidatorIndex), index0)
			require.Equal(t, int(res[1].Slot), slotEpoch*slotsPerEpoch)
			require.Equal(t, int(res[1].ValidatorIndex), index1)

			metadata := resp.Metadata
			require.Len(t, metadata, 2)
			require.Equal(t, true, metadata["execution_optimistic"])
			require.Equal(t, dependentRoot, metadata["dependent_root"].(eth2p0.Root))
		}

		testRouter(t, handler, callback)
	})

	t.Run("proposerduty", func(t *testing.T) {
		const total = 2

		handler := testHandler{
			ProposerDutiesFunc: func(ctx context.Context, opts *eth2api.ProposerDutiesOpts) (*eth2api.Response[[]*eth2v1.ProposerDuty], error) {
				// Returns ordered total number of duties for the epoch
				var res []*eth2v1.ProposerDuty
				for i := range total {
					res = append(res, &eth2v1.ProposerDuty{
						ValidatorIndex: eth2p0.ValidatorIndex(i),
						Slot:           eth2p0.Slot(int(opts.Epoch)*slotsPerEpoch + i),
					})
				}

				return wrapResponseWithMetadata(res, metadata), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			const (
				epoch     = 4
				validator = 1
			)

			opts := &eth2api.ProposerDutiesOpts{
				Epoch: eth2p0.Epoch(epoch),
				Indices: []eth2p0.ValidatorIndex{
					eth2p0.ValidatorIndex(validator), // Only request 1 of total 2 validators
				},
			}
			resp, err := cl.ProposerDuties(ctx, opts)
			require.NoError(t, err)

			res := resp.Data

			require.Len(t, res, 1)
			require.Equal(t, int(res[0].Slot), epoch*slotsPerEpoch+validator)
			require.Equal(t, int(res[0].ValidatorIndex), validator)

			metadata := resp.Metadata
			require.Len(t, metadata, 2)
			require.Equal(t, true, metadata["execution_optimistic"])
			require.Equal(t, dependentRoot, metadata["dependent_root"].(eth2p0.Root))
		}

		testRouter(t, handler, callback)
	})

	t.Run("synccommduty", func(t *testing.T) {
		handler := testHandler{
			SyncCommitteeDutiesFunc: func(ctx context.Context, opts *eth2api.SyncCommitteeDutiesOpts) (*eth2api.Response[[]*eth2v1.SyncCommitteeDuty], error) {
				// Returns ordered total number of duties for the epoch
				var res []*eth2v1.SyncCommitteeDuty
				for _, vIdx := range opts.Indices {
					res = append(res, &eth2v1.SyncCommitteeDuty{
						ValidatorIndex:                vIdx,
						ValidatorSyncCommitteeIndices: []eth2p0.CommitteeIndex{eth2p0.CommitteeIndex(vIdx)},
					})
				}

				return wrapResponse(res), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			const (
				epoch     = 4
				validator = 1
			)

			opts := &eth2api.SyncCommitteeDutiesOpts{
				Epoch: eth2p0.Epoch(epoch),
				Indices: []eth2p0.ValidatorIndex{
					eth2p0.ValidatorIndex(validator), // Only request 1 of total 2 validators
				},
			}
			resp, err := cl.SyncCommitteeDuties(ctx, opts)
			require.NoError(t, err)

			res := resp.Data

			require.Len(t, res, 1)
			require.Equal(t, res[0].ValidatorSyncCommitteeIndices, []eth2p0.CommitteeIndex{eth2p0.CommitteeIndex(validator)})
			require.Equal(t, int(res[0].ValidatorIndex), validator)
		}

		testRouter(t, handler, callback)
	})

	t.Run("get validator index", func(t *testing.T) {
		handler := testHandler{
			ValidatorsFunc: func(_ context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error) {
				res := make(map[eth2p0.ValidatorIndex]*eth2v1.Validator)
				for _, index := range opts.Indices {
					res[index] = &eth2v1.Validator{
						Index:  index,
						Status: eth2v1.ValidatorStateActiveOngoing,
						Validator: &eth2p0.Validator{
							PublicKey:             testutil.RandomEth2PubKey(t),
							WithdrawalCredentials: []byte("12345678901234567890123456789012"),
						},
					}
				}

				return wrapResponse(res), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			const (
				val1 = 1
				val2 = 2
			)

			opts := &eth2api.ValidatorsOpts{
				State: "head",
				Indices: []eth2p0.ValidatorIndex{
					eth2p0.ValidatorIndex(val1),
					eth2p0.ValidatorIndex(val2),
				},
			}
			resp, err := cl.Validators(ctx, opts)
			require.NoError(t, err)

			res := resp.Data

			require.Len(t, res, 2)
			require.EqualValues(t, val1, res[val1].Index)
			require.Equal(t, eth2v1.ValidatorStateActiveOngoing, res[val1].Status)
		}

		testRouter(t, handler, callback)
	})

	t.Run("get validator pubkey", func(t *testing.T) {
		var idx eth2p0.ValidatorIndex

		handler := testHandler{
			ValidatorsFunc: func(ctx context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error) {
				res := make(map[eth2p0.ValidatorIndex]*eth2v1.Validator)
				for _, pubkey := range opts.PubKeys {
					idx++
					res[idx] = &eth2v1.Validator{
						Index:  idx,
						Status: eth2v1.ValidatorStateActiveOngoing,
						Validator: &eth2p0.Validator{
							PublicKey:             pubkey,
							WithdrawalCredentials: []byte("12345678901234567890123456789012"),
						},
					}
				}

				return wrapResponse(res), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			opts := &eth2api.ValidatorsOpts{
				State: "head",
				PubKeys: []eth2p0.BLSPubKey{
					testutil.RandomEth2PubKey(t),
					testutil.RandomEth2PubKey(t),
				},
			}
			resp, err := cl.Validators(ctx, opts)
			require.NoError(t, err)

			res := resp.Data

			require.Len(t, res, 2)
			require.EqualValues(t, 1, res[1].Index)
			require.Equal(t, eth2v1.ValidatorStateActiveOngoing, res[1].Status)
		}

		testRouter(t, handler, callback)
	})

	t.Run("empty validators", func(t *testing.T) {
		handler := testHandler{
			ValidatorsFunc: func(ctx context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error) {
				return &eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator]{Data: nil}, nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			opts := &eth2api.ValidatorsOpts{
				State: "head",
				PubKeys: []eth2p0.BLSPubKey{
					testutil.RandomEth2PubKey(t),
					testutil.RandomEth2PubKey(t),
				},
			}
			resp, err := cl.Validators(ctx, opts)
			require.NoError(t, err)
			require.Empty(t, resp.Data)
		}

		testRouter(t, handler, callback)
	})

	t.Run("get validators with no validator ids provided", func(t *testing.T) {
		handler := testHandler{
			BeaconStateFunc: func(ctx context.Context, stateId string) (*eth2spec.VersionedBeaconState, error) {
				return testutil.RandomBeaconState(t), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			opts := &eth2api.ValidatorsOpts{
				State: "head",
			}
			resp, err := cl.Validators(ctx, opts)
			require.NoError(t, err)

			res := resp.Data

			// Two validators are expected as the testutil.RandomBeaconState(t) returns two validators.
			require.Len(t, res, 2)
		}

		testRouter(t, handler, callback)
	})

	t.Run("empty attester duties", func(t *testing.T) {
		handler := testHandler{
			AttesterDutiesFunc: func(ctx context.Context, opts *eth2api.AttesterDutiesOpts) (*eth2api.Response[[]*eth2v1.AttesterDuty], error) {
				return &eth2api.Response[[]*eth2v1.AttesterDuty]{Metadata: metadata}, nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			opts := &eth2api.AttesterDutiesOpts{
				Epoch:   eth2p0.Epoch(1),
				Indices: []eth2p0.ValidatorIndex{1, 2, 3},
			}
			resp, err := cl.AttesterDuties(ctx, opts)
			require.NoError(t, err)
			require.Empty(t, resp.Data)
		}

		testRouter(t, handler, callback)
	})

	t.Run("empty synccomm duties", func(t *testing.T) {
		handler := testHandler{
			SyncCommitteeDutiesFunc: func(ctx context.Context, opts *eth2api.SyncCommitteeDutiesOpts) (*eth2api.Response[[]*eth2v1.SyncCommitteeDuty], error) {
				return &eth2api.Response[[]*eth2v1.SyncCommitteeDuty]{}, nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			opts := &eth2api.SyncCommitteeDutiesOpts{
				Epoch:   eth2p0.Epoch(1),
				Indices: []eth2p0.ValidatorIndex{1, 2, 3},
			}
			res, err := cl.SyncCommitteeDuties(ctx, opts)
			require.NoError(t, err)
			require.Empty(t, res.Data)
		}

		testRouter(t, handler, callback)
	})

	t.Run("empty proposer duties", func(t *testing.T) {
		handler := testHandler{
			ProposerDutiesFunc: func(ctx context.Context, opts *eth2api.ProposerDutiesOpts) (*eth2api.Response[[]*eth2v1.ProposerDuty], error) {
				return &eth2api.Response[[]*eth2v1.ProposerDuty]{Metadata: metadata}, nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			opts := &eth2api.ProposerDutiesOpts{
				Epoch:   eth2p0.Epoch(1),
				Indices: []eth2p0.ValidatorIndex{1, 2, 3},
			}
			res, err := cl.ProposerDuties(ctx, opts)
			require.NoError(t, err)
			require.Empty(t, res.Data)
		}

		testRouter(t, handler, callback)
	})

	t.Run("attestation data", func(t *testing.T) {
		handler := testHandler{
			AttestationDataFunc: func(ctx context.Context, opts *eth2api.AttestationDataOpts) (*eth2api.Response[*eth2p0.AttestationData], error) {
				data := testutil.RandomAttestationDataPhase0()
				data.Slot = opts.Slot
				data.Index = opts.CommitteeIndex

				return wrapResponse(data), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			const slot, commIdx = 12, 23

			opts := &eth2api.AttestationDataOpts{
				Slot:           slot,
				CommitteeIndex: commIdx,
			}
			res, err := cl.AttestationData(ctx, opts)
			require.NoError(t, err)

			require.EqualValues(t, slot, res.Data.Slot)
			require.EqualValues(t, commIdx, res.Data.Index)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit randao", func(t *testing.T) {
		handler := testHandler{
			ProposalFunc: func(ctx context.Context, opts *eth2api.ProposalOpts) (*eth2api.Response[*eth2api.VersionedProposal], error) {
				return &eth2api.Response[*eth2api.VersionedProposal]{Data: nil}, errors.New("not implemented")
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			slot := eth2p0.Slot(1)
			randaoReveal := testutil.RandomEth2Signature()
			graffiti := testutil.RandomArray32()

			opts := &eth2api.ProposalOpts{
				Slot:         slot,
				RandaoReveal: randaoReveal,
				Graffiti:     graffiti,
			}
			res, err := cl.Proposal(ctx, opts)
			require.Error(t, err)
			require.Nil(t, res)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit block phase0", func(t *testing.T) {
		block1 := &eth2api.VersionedSignedProposal{
			Version: eth2spec.DataVersionPhase0,
			Phase0: &eth2p0.SignedBeaconBlock{
				Message:   testutil.RandomPhase0BeaconBlock(),
				Signature: testutil.RandomEth2Signature(),
			},
		}
		handler := testHandler{
			SubmitProposalFunc: func(ctx context.Context, block *eth2api.SubmitProposalOpts) error {
				require.Equal(t, block.Proposal, block1)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitProposal(ctx, &eth2api.SubmitProposalOpts{
				Proposal: block1,
			})
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit block altair", func(t *testing.T) {
		block1 := &eth2api.VersionedSignedProposal{
			Version: eth2spec.DataVersionAltair,
			Altair: &altair.SignedBeaconBlock{
				Message:   testutil.RandomAltairBeaconBlock(),
				Signature: testutil.RandomEth2Signature(),
			},
		}
		handler := testHandler{
			SubmitProposalFunc: func(ctx context.Context, block *eth2api.SubmitProposalOpts) error {
				require.Equal(t, block.Proposal, block1)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitProposal(ctx, &eth2api.SubmitProposalOpts{
				Proposal: block1,
			})
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit block bellatrix", func(t *testing.T) {
		block1 := &eth2api.VersionedSignedProposal{
			Version: eth2spec.DataVersionBellatrix,
			Bellatrix: &bellatrix.SignedBeaconBlock{
				Message:   testutil.RandomBellatrixBeaconBlock(),
				Signature: testutil.RandomEth2Signature(),
			},
		}
		handler := testHandler{
			SubmitProposalFunc: func(ctx context.Context, block *eth2api.SubmitProposalOpts) error {
				require.Equal(t, block.Proposal, block1)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitProposal(ctx, &eth2api.SubmitProposalOpts{
				Proposal: block1,
			})
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit block capella", func(t *testing.T) {
		block1 := &eth2api.VersionedSignedProposal{
			Version: eth2spec.DataVersionCapella,
			Capella: &capella.SignedBeaconBlock{
				Message:   testutil.RandomCapellaBeaconBlock(),
				Signature: testutil.RandomEth2Signature(),
			},
		}
		handler := testHandler{
			SubmitProposalFunc: func(ctx context.Context, block *eth2api.SubmitProposalOpts) error {
				require.Equal(t, block.Proposal, block1)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitProposal(ctx, &eth2api.SubmitProposalOpts{
				Proposal: block1,
			})
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit blinded block bellatrix", func(t *testing.T) {
		block1 := &eth2api.VersionedSignedBlindedProposal{
			Version: eth2spec.DataVersionBellatrix,
			Bellatrix: &eth2bellatrix.SignedBlindedBeaconBlock{
				Message:   testutil.RandomBellatrixBlindedBeaconBlock(),
				Signature: testutil.RandomEth2Signature(),
			},
		}
		handler := testHandler{
			SubmitBlindedProposalFunc: func(ctx context.Context, block *eth2api.SubmitBlindedProposalOpts) error {
				require.Equal(t, block.Proposal, block1)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitBlindedProposal(ctx, &eth2api.SubmitBlindedProposalOpts{
				Proposal: block1,
			})
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit blinded block capella", func(t *testing.T) {
		block1 := &eth2api.VersionedSignedBlindedProposal{
			Version: eth2spec.DataVersionCapella,
			Capella: &eth2capella.SignedBlindedBeaconBlock{
				Message:   testutil.RandomCapellaBlindedBeaconBlock(),
				Signature: testutil.RandomEth2Signature(),
			},
		}
		handler := testHandler{
			SubmitBlindedProposalFunc: func(ctx context.Context, block *eth2api.SubmitBlindedProposalOpts) error {
				require.Equal(t, block1, block.Proposal)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitBlindedProposal(ctx, &eth2api.SubmitBlindedProposalOpts{
				Proposal: block1,
			})
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit blinded block deneb", func(t *testing.T) {
		block1 := &eth2api.VersionedSignedBlindedProposal{
			Version: eth2spec.DataVersionDeneb,
			Deneb: &eth2deneb.SignedBlindedBeaconBlock{
				Message:   testutil.RandomDenebBlindedBeaconBlock(),
				Signature: testutil.RandomEth2Signature(),
			},
		}
		handler := testHandler{
			SubmitBlindedProposalFunc: func(ctx context.Context, block *eth2api.SubmitBlindedProposalOpts) error {
				require.Equal(t, block1, block.Proposal)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitBlindedProposal(ctx, &eth2api.SubmitBlindedProposalOpts{
				Proposal: block1,
			})
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit blinded block electra", func(t *testing.T) {
		block1 := &eth2api.VersionedSignedBlindedProposal{
			Version: eth2spec.DataVersionElectra,
			Electra: &eth2electra.SignedBlindedBeaconBlock{
				Message:   testutil.RandomElectraBlindedBeaconBlock(),
				Signature: testutil.RandomEth2Signature(),
			},
		}
		handler := testHandler{
			SubmitBlindedProposalFunc: func(ctx context.Context, block *eth2api.SubmitBlindedProposalOpts) error {
				require.Equal(t, block1, block.Proposal)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitBlindedProposal(ctx, &eth2api.SubmitBlindedProposalOpts{
				Proposal: block1,
			})
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit validator registration", func(t *testing.T) {
		expect := []*eth2api.VersionedSignedValidatorRegistration{
			{
				Version: eth2spec.BuilderVersionV1,
				V1:      testutil.RandomSignedValidatorRegistration(t),
			},
		}
		handler := testHandler{
			SubmitValidatorRegistrationsFunc: func(ctx context.Context, actual []*eth2api.VersionedSignedValidatorRegistration) error {
				require.Equal(t, actual, expect)

				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitValidatorRegistrations(ctx, expect)
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit voluntary exit", func(t *testing.T) {
		exit1 := testutil.RandomExit()

		handler := testHandler{
			SubmitVoluntaryExitFunc: func(ctx context.Context, exit2 *eth2p0.SignedVoluntaryExit) error {
				require.Equal(t, *exit1, *exit2)
				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			err := cl.SubmitVoluntaryExit(ctx, exit1)
			require.NoError(t, err)
		}

		testRouter(t, handler, callback)
	})

	t.Run("sync committee contribution", func(t *testing.T) {
		handler := testHandler{
			SyncCommitteeContributionFunc: func(ctx context.Context, opts *eth2api.SyncCommitteeContributionOpts) (*eth2api.Response[*altair.SyncCommitteeContribution], error) {
				contrib := testutil.RandomSyncCommitteeContribution()
				contrib.Slot = opts.Slot
				contrib.SubcommitteeIndex = opts.SubcommitteeIndex
				contrib.BeaconBlockRoot = opts.BeaconBlockRoot

				return wrapResponse(contrib), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			var (
				slot            = testutil.RandomSlot()
				subcommIdx      = testutil.RandomCommIdx()
				beaconBlockRoot = testutil.RandomRoot()
			)

			opts := &eth2api.SyncCommitteeContributionOpts{
				Slot:              slot,
				SubcommitteeIndex: uint64(subcommIdx),
				BeaconBlockRoot:   beaconBlockRoot,
			}
			resp, err := cl.SyncCommitteeContribution(ctx, opts)
			require.NoError(t, err)

			require.Equal(t, resp.Data.Slot, slot)
			require.EqualValues(t, resp.Data.SubcommitteeIndex, subcommIdx)
			require.Equal(t, resp.Data.BeaconBlockRoot, beaconBlockRoot)
		}

		testRouter(t, handler, callback)
	})

	t.Run("submit sync committee messages", func(t *testing.T) {
		msgs := []*altair.SyncCommitteeMessage{testutil.RandomSyncCommitteeMessage(), testutil.RandomSyncCommitteeMessage()}

		handler := testHandler{
			SubmitSyncCommitteeMessagesFunc: func(ctx context.Context, messages []*altair.SyncCommitteeMessage) error {
				for i := range msgs {
					require.Equal(t, msgs[i], messages[i])
				}

				return nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			require.NoError(t, cl.SubmitSyncCommitteeMessages(ctx, msgs))
		}

		testRouter(t, handler, callback)
	})

	t.Run("aggregate sync committee selections", func(t *testing.T) {
		selections := []*eth2exp.SyncCommitteeSelection{testutil.RandomSyncCommitteeSelection(), testutil.RandomSyncCommitteeSelection()}

		handler := testHandler{
			AggregateSyncCommitteeSelectionsFunc: func(ctx context.Context, partialSelections []*eth2exp.SyncCommitteeSelection) ([]*eth2exp.SyncCommitteeSelection, error) {
				for i := range selections {
					require.Equal(t, selections[i], partialSelections[i])
				}

				return partialSelections, nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			eth2Cl := eth2wrap.AdaptEth2HTTP(cl, nil, time.Second)
			actual, err := eth2Cl.AggregateSyncCommitteeSelections(ctx, selections)
			require.NoError(t, err)
			require.Equal(t, selections, actual)
		}

		testRouter(t, handler, callback)
	})

	t.Run("node version", func(t *testing.T) {
		expectedVersion := "obolnetwork/charon/v0.25.0-eth123b/darwin-arm64"

		handler := testHandler{
			NodeVersionFunc: func(ctx context.Context, opts *eth2api.NodeVersionOpts) (*eth2api.Response[string], error) {
				return wrapResponse(expectedVersion), nil
			},
		}

		callback := func(ctx context.Context, cl *eth2http.Service) {
			eth2Resp, err := cl.NodeVersion(ctx, &eth2api.NodeVersionOpts{})
			require.NoError(t, err)

			actualVersion := eth2Resp.Data
			require.Equal(t, expectedVersion, actualVersion)
		}

		testRouter(t, handler, callback)
	})
}

func TestBeaconCommitteeSelections(t *testing.T) {
	ctx := context.Background()

	const (
		slotA = 123
		slotB = 456
		vIdxA = 1
		vIdxB = 2
		vIdxC = 3
	)

	handler := testHandler{
		AggregateBeaconCommitteeSelectionsFunc: func(ctx context.Context, selections []*eth2exp.BeaconCommitteeSelection) ([]*eth2exp.BeaconCommitteeSelection, error) {
			return selections, nil
		},
	}

	proxy := httptest.NewServer(handler.newBeaconHandler(t))
	defer proxy.Close()

	r, err := NewRouter(ctx, handler, testBeaconAddr{addr: proxy.URL}, true)
	require.NoError(t, err)

	server := httptest.NewServer(r)
	defer server.Close()

	var eth2Svc eth2client.Service

	eth2Svc, err = eth2http.New(ctx,
		eth2http.WithLogLevel(1),
		eth2http.WithAddress(server.URL),
	)
	require.NoError(t, err)

	selections := []*eth2exp.BeaconCommitteeSelection{
		{
			Slot:           slotA,
			ValidatorIndex: vIdxA,
			SelectionProof: testutil.RandomEth2Signature(),
		},
		{
			Slot:           slotB,
			ValidatorIndex: vIdxB,
			SelectionProof: testutil.RandomEth2Signature(),
		},
		{
			Slot:           slotA,
			ValidatorIndex: vIdxC,
			SelectionProof: testutil.RandomEth2Signature(),
		},
	}

	eth2Cl := eth2wrap.AdaptEth2HTTP(eth2Svc.(*eth2http.Service), nil, time.Second)
	actual, err := eth2Cl.AggregateBeaconCommitteeSelections(ctx, selections)
	require.NoError(t, err)
	require.Equal(t, selections, actual)
}

func TestSubmitAggregateAttestations(t *testing.T) {
	const vIdx = 1

	tests := []struct {
		version                          eth2spec.DataVersion
		versionedSignedAggregateAndProof *eth2spec.VersionedSignedAggregateAndProof
	}{
		{
			version: eth2spec.DataVersionElectra,
			versionedSignedAggregateAndProof: &eth2spec.VersionedSignedAggregateAndProof{
				Version: eth2spec.DataVersionElectra,
				Electra: &electra.SignedAggregateAndProof{
					Message: &electra.AggregateAndProof{
						AggregatorIndex: vIdx,
						Aggregate:       testutil.RandomElectraAttestation(),
						SelectionProof:  testutil.RandomEth2Signature(),
					},
					Signature: testutil.RandomEth2Signature(),
				},
			},
		},
		{
			version: eth2spec.DataVersionElectra,
			versionedSignedAggregateAndProof: &eth2spec.VersionedSignedAggregateAndProof{
				Version: eth2spec.DataVersionElectra,
				Electra: &electra.SignedAggregateAndProof{
					Message: &electra.AggregateAndProof{
						AggregatorIndex: vIdx,
						Aggregate:       testutil.RandomElectraAttestation(),
						SelectionProof:  testutil.RandomEth2Signature(),
					},
					Signature: testutil.RandomEth2Signature(),
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.version.String(), func(t *testing.T) {
			ctx := context.Background()

			agg := test.versionedSignedAggregateAndProof

			handler := testHandler{
				SubmitAggregateAttestationsFunc: func(_ context.Context, aggregateAndProofs *eth2api.SubmitAggregateAttestationsOpts) error {
					require.Equal(t, agg, aggregateAndProofs.SignedAggregateAndProofs[0])

					return nil
				},
			}

			proxy := httptest.NewServer(handler.newBeaconHandler(t))
			defer proxy.Close()

			r, err := NewRouter(ctx, handler, testBeaconAddr{addr: proxy.URL}, true)
			require.NoError(t, err)

			server := httptest.NewServer(r)
			defer server.Close()

			var eth2Svc eth2client.Service

			eth2Svc, err = eth2http.New(ctx,
				eth2http.WithLogLevel(1),
				eth2http.WithAddress(server.URL),
			)
			require.NoError(t, err)

			eth2Cl := eth2wrap.AdaptEth2HTTP(eth2Svc.(*eth2http.Service), nil, time.Second)
			err = eth2Cl.SubmitAggregateAttestations(ctx, &eth2api.SubmitAggregateAttestationsOpts{SignedAggregateAndProofs: []*eth2spec.VersionedSignedAggregateAndProof{agg}})
			require.NoError(t, err)
		})
	}
}

func TestSubmitAttestations(t *testing.T) {
	vidx := testutil.RandomVIdx()

	tests := []struct {
		version              eth2spec.DataVersion
		versionedAttestation *eth2spec.VersionedAttestation
	}{
		{
			version: eth2spec.DataVersionDeneb,
			versionedAttestation: &eth2spec.VersionedAttestation{
				Version: eth2spec.DataVersionDeneb,
				Deneb:   testutil.RandomPhase0Attestation(),
			},
		},
		{
			version: eth2spec.DataVersionElectra,
			versionedAttestation: &eth2spec.VersionedAttestation{
				Version:        eth2spec.DataVersionElectra,
				ValidatorIndex: &vidx,
				Electra:        testutil.RandomElectraAttestation(),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.version.String(), func(t *testing.T) {
			ctx := context.Background()

			att := test.versionedAttestation

			handler := testHandler{
				SubmitAttestationsFunc: func(_ context.Context, attestations *eth2api.SubmitAttestationsOpts) error {
					switch attestations.Attestations[0].Version {
					case eth2spec.DataVersionPhase0:
						require.Equal(t, att, attestations.Attestations[0])
					case eth2spec.DataVersionAltair:
						require.Equal(t, att, attestations.Attestations[0])
					case eth2spec.DataVersionBellatrix:
						require.Equal(t, att, attestations.Attestations[0])
					case eth2spec.DataVersionCapella:
						require.Equal(t, att, attestations.Attestations[0])
					case eth2spec.DataVersionDeneb:
						require.Equal(t, att, attestations.Attestations[0])
					case eth2spec.DataVersionElectra:
						// we don't check for aggregation bits for electra, as it uses SingleAttestation structure which does not include them aggregation bits
						require.Equal(t, att.Electra.Data, attestations.Attestations[0].Electra.Data)
						require.Equal(t, att.Electra.Signature, attestations.Attestations[0].Electra.Signature)
						require.Equal(t, att.Electra.CommitteeBits, attestations.Attestations[0].Electra.CommitteeBits)
					default:
						require.Fail(t, "unknown version")
					}

					return nil
				},
			}

			proxy := httptest.NewServer(handler.newBeaconHandler(t))
			defer proxy.Close()

			r, err := NewRouter(ctx, handler, testBeaconAddr{addr: proxy.URL}, true)
			require.NoError(t, err)

			server := httptest.NewServer(r)
			defer server.Close()

			var eth2Svc eth2client.Service

			eth2Svc, err = eth2http.New(ctx,
				eth2http.WithLogLevel(1),
				eth2http.WithAddress(server.URL),
			)
			require.NoError(t, err)

			eth2Cl := eth2wrap.AdaptEth2HTTP(eth2Svc.(*eth2http.Service), nil, time.Second)
			err = eth2Cl.SubmitAttestations(ctx, &eth2api.SubmitAttestationsOpts{Attestations: []*eth2spec.VersionedAttestation{att}})
			require.NoError(t, err)
		})
	}
}

func TestGetExecutionOptimisticFromMetadata(t *testing.T) {
	t.Run("missing execution_optimistic", func(t *testing.T) {
		metadata := map[string]any{}

		_, err := getExecutionOptimisticFromMetadata(metadata)

		require.ErrorContains(t, err, "metadata has missing execution_optimistic value")
	})

	t.Run("wrong type", func(t *testing.T) {
		metadata := map[string]any{
			"execution_optimistic": "not-a-bool",
		}

		_, err := getExecutionOptimisticFromMetadata(metadata)

		require.ErrorContains(t, err, "metadata has malformed execution_optimistic value")
	})

	t.Run("valid value", func(t *testing.T) {
		metadata := map[string]any{
			"execution_optimistic": true,
		}

		executionOptimistic, err := getExecutionOptimisticFromMetadata(metadata)

		require.NoError(t, err)
		require.True(t, executionOptimistic)
	})
}

func TestGetDependentRootFromMetadata(t *testing.T) {
	t.Run("missing dependent_root", func(t *testing.T) {
		metadata := map[string]any{}

		_, err := getDependentRootFromMetadata(metadata)

		require.ErrorContains(t, err, "metadata has missing dependent_root value")
	})

	t.Run("wrong type", func(t *testing.T) {
		metadata := map[string]any{
			"dependent_root": 123,
		}

		_, err := getDependentRootFromMetadata(metadata)

		require.ErrorContains(t, err, "metadata has wrong dependent_root type")
	})

	t.Run("valid value", func(t *testing.T) {
		var r eth2p0.Root

		_, _ = rand.Read(r[:])

		metadata := map[string]any{
			"dependent_root": r,
		}

		dependentRoot, err := getDependentRootFromMetadata(metadata)

		require.NoError(t, err)
		require.Equal(t, r, eth2p0.Root(dependentRoot))
	})
}

func TestCreateProposeBlindedBlockResponse(t *testing.T) {
	p := &eth2api.VersionedProposal{
		Version: eth2spec.DataVersionPhase0,
		Phase0:  testutil.RandomPhase0BeaconBlock(),
		Blinded: true,
	}

	_, err := createProposeBlockResponse(p)
	require.ErrorContains(t, err, "invalid blinded block")

	t.Run("bellatrix", func(t *testing.T) {
		p = &eth2api.VersionedProposal{
			Version:          eth2spec.DataVersionBellatrix,
			BellatrixBlinded: testutil.RandomBellatrixBlindedBeaconBlock(),
			Blinded:          true,
			ConsensusValue:   big.NewInt(123),
			ExecutionValue:   big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.BellatrixBlinded, pp.Data)
		require.Equal(t, p.ConsensusValue.String(), pp.ConsensusBlockValue)
		require.Equal(t, p.ExecutionValue.String(), pp.ExecutionPayloadValue)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionBellatrix,
			Blinded: true,
		})
		require.ErrorContains(t, err, "no bellatrix blinded block")
	})

	t.Run("capella", func(t *testing.T) {
		p := &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionCapella,
			CapellaBlinded: testutil.RandomCapellaBlindedBeaconBlock(),
			Blinded:        true,
			ConsensusValue: big.NewInt(123),
			ExecutionValue: big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.CapellaBlinded, pp.Data)
		require.Equal(t, p.ConsensusValue.String(), pp.ConsensusBlockValue)
		require.Equal(t, p.ExecutionValue.String(), pp.ExecutionPayloadValue)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionCapella,
			Blinded: true,
		})
		require.ErrorContains(t, err, "no capella blinded block")
	})

	t.Run("deneb", func(t *testing.T) {
		p := &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionDeneb,
			DenebBlinded:   testutil.RandomDenebBlindedBeaconBlock(),
			Blinded:        true,
			ConsensusValue: big.NewInt(123),
			ExecutionValue: big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.DenebBlinded, pp.Data)
		require.Equal(t, p.ConsensusValue.String(), pp.ConsensusBlockValue)
		require.Equal(t, p.ExecutionValue.String(), pp.ExecutionPayloadValue)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionDeneb,
			Blinded: true,
		})
		require.ErrorContains(t, err, "no deneb blinded block")
	})

	t.Run("electra", func(t *testing.T) {
		p := &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionElectra,
			ElectraBlinded: testutil.RandomElectraBlindedBeaconBlock(),
			Blinded:        true,
			ConsensusValue: big.NewInt(123),
			ExecutionValue: big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.ElectraBlinded, pp.Data)
		require.Equal(t, p.ConsensusValue.String(), pp.ConsensusBlockValue)
		require.Equal(t, p.ExecutionValue.String(), pp.ExecutionPayloadValue)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionElectra,
			Blinded: true,
		})
		require.ErrorContains(t, err, "no electra blinded block")
	})
}

func TestCreateProposeBlockResponse(t *testing.T) {
	p := &eth2api.VersionedProposal{
		Version: eth2spec.DataVersionUnknown,
	}

	_, err := createProposeBlockResponse(p)
	require.ErrorContains(t, err, "invalid block")

	t.Run("phase0", func(t *testing.T) {
		p = &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionPhase0,
			Phase0:         testutil.RandomPhase0BeaconBlock(),
			ConsensusValue: big.NewInt(123),
			ExecutionValue: big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.Phase0, pp.Data)
		require.Equal(t, p.ConsensusValue.String(), pp.ConsensusBlockValue)
		require.Equal(t, p.ExecutionValue.String(), pp.ExecutionPayloadValue)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionPhase0,
		})
		require.ErrorContains(t, err, "no phase0 block")
	})

	t.Run("altair", func(t *testing.T) {
		p = &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionAltair,
			Altair:         testutil.RandomAltairBeaconBlock(),
			ConsensusValue: big.NewInt(123),
			ExecutionValue: big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.Altair, pp.Data)
		require.Equal(t, p.ConsensusValue.String(), pp.ConsensusBlockValue)
		require.Equal(t, p.ExecutionValue.String(), pp.ExecutionPayloadValue)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionAltair,
		})
		require.ErrorContains(t, err, "no altair block")
	})

	t.Run("bellatrix", func(t *testing.T) {
		p = &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionBellatrix,
			Bellatrix:      testutil.RandomBellatrixBeaconBlock(),
			ConsensusValue: big.NewInt(123),
			ExecutionValue: big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.Bellatrix, pp.Data)
		require.Equal(t, p.ConsensusValue.String(), pp.ConsensusBlockValue)
		require.Equal(t, p.ExecutionValue.String(), pp.ExecutionPayloadValue)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionBellatrix,
		})
		require.ErrorContains(t, err, "no bellatrix block")
	})

	t.Run("capella", func(t *testing.T) {
		p := &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionCapella,
			Capella:        testutil.RandomCapellaBeaconBlock(),
			ConsensusValue: big.NewInt(123),
			ExecutionValue: big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.Capella, pp.Data)
		require.Equal(t, p.ConsensusValue.String(), pp.ConsensusBlockValue)
		require.Equal(t, p.ExecutionValue.String(), pp.ExecutionPayloadValue)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionCapella,
		})
		require.ErrorContains(t, err, "no capella block")
	})

	t.Run("deneb", func(t *testing.T) {
		p := &eth2api.VersionedProposal{
			Version:        eth2spec.DataVersionDeneb,
			Deneb:          testutil.RandomDenebVersionedProposal().Deneb,
			ConsensusValue: big.NewInt(123),
			ExecutionValue: big.NewInt(456),
		}

		pp, err := createProposeBlockResponse(p)
		require.NoError(t, err)
		require.NotNil(t, pp)
		require.Equal(t, p.Version.String(), pp.Version)
		require.Equal(t, p.Deneb, pp.Data)

		_, err = createProposeBlockResponse(&eth2api.VersionedProposal{
			Version: eth2spec.DataVersionDeneb,
		})
		require.ErrorContains(t, err, "no deneb block")
	})
}

// testRouter is a helper function to test router endpoints with an eth2http client. The outer test
// provides the mocked test handler and a callback that does the client side test.
func testRouter(t *testing.T, handler testHandler, callback func(context.Context, *eth2http.Service)) {
	t.Helper()

	proxy := httptest.NewServer(handler.newBeaconHandler(t))
	defer proxy.Close()

	ctx := context.Background()

	r, err := NewRouter(ctx, handler, testBeaconAddr{addr: proxy.URL}, true)
	require.NoError(t, err)

	server := httptest.NewServer(r)
	defer server.Close()

	cl, err := eth2http.New(ctx, eth2http.WithAddress(server.URL), eth2http.WithLogLevel(infoLevel))
	require.NoError(t, err)

	callback(ctx, cl.(*eth2http.Service))
}

// testRawRouter is a helper function to test router endpoints with a raw http client. The outer test
// provides the mocked test handler and a callback that does the client side test.
// The router is configured with BuilderAPI always enabled.
func testRawRouter(t *testing.T, handler testHandler, callback func(context.Context, string)) {
	t.Helper()

	testRawRouterEx(t, handler, callback, true)
}

// testRawRouterEx is a helper function same as testRawRouter() but accepts GetBuilderAPIFlagFunc.
func testRawRouterEx(t *testing.T, handler testHandler, callback func(context.Context, string), builderEnabled bool) {
	t.Helper()

	proxy := httptest.NewServer(handler.newBeaconHandler(t))
	defer proxy.Close()

	r, err := NewRouter(context.Background(), handler, testBeaconAddr{addr: proxy.URL}, builderEnabled)
	require.NoError(t, err)

	server := httptest.NewServer(r)
	defer server.Close()

	callback(context.Background(), server.URL)
}

// testHandler implements the Handler interface allowing test-cases to specify only what they require.
// This includes optional validatorapi handler functions, an optional beacon-node reserve proxy handler, and
// mocked beacon-node endpoints required by the eth2http client during startup.
type testHandler struct {
	Handler
	eth2client.BeaconStateProvider

	ProxyHandler                           http.HandlerFunc
	AggregateSyncCommitteeSelectionsFunc   func(ctx context.Context, partialSelections []*eth2exp.SyncCommitteeSelection) ([]*eth2exp.SyncCommitteeSelection, error)
	AttestationDataFunc                    func(ctx context.Context, opts *eth2api.AttestationDataOpts) (*eth2api.Response[*eth2p0.AttestationData], error)
	AttesterDutiesFunc                     func(ctx context.Context, opts *eth2api.AttesterDutiesOpts) (*eth2api.Response[[]*eth2v1.AttesterDuty], error)
	SubmitAttestationsFunc                 func(ctx context.Context, opts *eth2api.SubmitAttestationsOpts) error
	ProposalFunc                           func(ctx context.Context, opts *eth2api.ProposalOpts) (*eth2api.Response[*eth2api.VersionedProposal], error)
	SubmitProposalFunc                     func(ctx context.Context, proposal *eth2api.SubmitProposalOpts) error
	SubmitBlindedProposalFunc              func(ctx context.Context, proposal *eth2api.SubmitBlindedProposalOpts) error
	ProposerDutiesFunc                     func(ctx context.Context, opts *eth2api.ProposerDutiesOpts) (*eth2api.Response[[]*eth2v1.ProposerDuty], error)
	NodeVersionFunc                        func(ctx context.Context, opts *eth2api.NodeVersionOpts) (*eth2api.Response[string], error)
	ValidatorsFunc                         func(ctx context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error)
	BeaconStateFunc                        func(ctx context.Context, stateId string) (*eth2spec.VersionedBeaconState, error)
	ValidatorsByPubKeyFunc                 func(ctx context.Context, stateID string, pubkeys []eth2p0.BLSPubKey) (map[eth2p0.ValidatorIndex]*eth2v1.Validator, error)
	SubmitVoluntaryExitFunc                func(ctx context.Context, exit *eth2p0.SignedVoluntaryExit) error
	SubmitValidatorRegistrationsFunc       func(ctx context.Context, registrations []*eth2api.VersionedSignedValidatorRegistration) error
	AggregateBeaconCommitteeSelectionsFunc func(ctx context.Context, selections []*eth2exp.BeaconCommitteeSelection) ([]*eth2exp.BeaconCommitteeSelection, error)
	SubmitAggregateAttestationsFunc        func(ctx context.Context, opts *eth2api.SubmitAggregateAttestationsOpts) error
	SubmitSyncCommitteeMessagesFunc        func(ctx context.Context, messages []*altair.SyncCommitteeMessage) error
	SyncCommitteeDutiesFunc                func(ctx context.Context, opts *eth2api.SyncCommitteeDutiesOpts) (*eth2api.Response[[]*eth2v1.SyncCommitteeDuty], error)
	SyncCommitteeContributionFunc          func(ctx context.Context, opts *eth2api.SyncCommitteeContributionOpts) (*eth2api.Response[*altair.SyncCommitteeContribution], error)
}

func (h testHandler) AttestationData(ctx context.Context, opts *eth2api.AttestationDataOpts) (*eth2api.Response[*eth2p0.AttestationData], error) {
	return h.AttestationDataFunc(ctx, opts)
}

func (h testHandler) AttesterDuties(ctx context.Context, opts *eth2api.AttesterDutiesOpts) (*eth2api.Response[[]*eth2v1.AttesterDuty], error) {
	return h.AttesterDutiesFunc(ctx, opts)
}

func (h testHandler) SubmitAttestations(ctx context.Context, opts *eth2api.SubmitAttestationsOpts) error {
	return h.SubmitAttestationsFunc(ctx, opts)
}

func (h testHandler) Proposal(ctx context.Context, opts *eth2api.ProposalOpts) (*eth2api.Response[*eth2api.VersionedProposal], error) {
	return h.ProposalFunc(ctx, opts)
}

func (h testHandler) SubmitProposal(ctx context.Context, proposal *eth2api.SubmitProposalOpts) error {
	return h.SubmitProposalFunc(ctx, proposal)
}

func (h testHandler) SubmitBlindedProposal(ctx context.Context, block *eth2api.SubmitBlindedProposalOpts) error {
	return h.SubmitBlindedProposalFunc(ctx, block)
}

func (h testHandler) Validators(ctx context.Context, opts *eth2api.ValidatorsOpts) (*eth2api.Response[map[eth2p0.ValidatorIndex]*eth2v1.Validator], error) {
	return h.ValidatorsFunc(ctx, opts)
}

func (h testHandler) BeaconState(ctx context.Context, stateID string) (*eth2spec.VersionedBeaconState, error) {
	return h.BeaconStateFunc(ctx, stateID)
}

func (h testHandler) ValidatorsByPubKey(ctx context.Context, stateID string, pubkeys []eth2p0.BLSPubKey) (map[eth2p0.ValidatorIndex]*eth2v1.Validator, error) {
	return h.ValidatorsByPubKeyFunc(ctx, stateID, pubkeys)
}

func (h testHandler) ProposerDuties(ctx context.Context, opts *eth2api.ProposerDutiesOpts) (*eth2api.Response[[]*eth2v1.ProposerDuty], error) {
	return h.ProposerDutiesFunc(ctx, opts)
}

func (h testHandler) NodeVersion(ctx context.Context, opts *eth2api.NodeVersionOpts) (*eth2api.Response[string], error) {
	if h.NodeVersionFunc != nil {
		return h.NodeVersionFunc(ctx, opts)
	}

	return wrapResponse("mock_version"), nil
}

func (h testHandler) SubmitVoluntaryExit(ctx context.Context, exit *eth2p0.SignedVoluntaryExit) error {
	return h.SubmitVoluntaryExitFunc(ctx, exit)
}

func (h testHandler) SubmitValidatorRegistrations(ctx context.Context, registrations []*eth2api.VersionedSignedValidatorRegistration) error {
	return h.SubmitValidatorRegistrationsFunc(ctx, registrations)
}

func (h testHandler) AggregateBeaconCommitteeSelections(ctx context.Context, selections []*eth2exp.BeaconCommitteeSelection) ([]*eth2exp.BeaconCommitteeSelection, error) {
	return h.AggregateBeaconCommitteeSelectionsFunc(ctx, selections)
}

func (h testHandler) SubmitAggregateAttestations(ctx context.Context, opts *eth2api.SubmitAggregateAttestationsOpts) error {
	return h.SubmitAggregateAttestationsFunc(ctx, opts)
}

func (h testHandler) SubmitSyncCommitteeMessages(ctx context.Context, messages []*altair.SyncCommitteeMessage) error {
	return h.SubmitSyncCommitteeMessagesFunc(ctx, messages)
}

func (h testHandler) SyncCommitteeDuties(ctx context.Context, opts *eth2api.SyncCommitteeDutiesOpts) (*eth2api.Response[[]*eth2v1.SyncCommitteeDuty], error) {
	return h.SyncCommitteeDutiesFunc(ctx, opts)
}

func (h testHandler) SyncCommitteeContribution(ctx context.Context, opts *eth2api.SyncCommitteeContributionOpts) (*eth2api.Response[*altair.SyncCommitteeContribution], error) {
	return h.SyncCommitteeContributionFunc(ctx, opts)
}

func (h testHandler) AggregateSyncCommitteeSelections(ctx context.Context, partialSelections []*eth2exp.SyncCommitteeSelection) ([]*eth2exp.SyncCommitteeSelection, error) {
	return h.AggregateSyncCommitteeSelectionsFunc(ctx, partialSelections)
}

// newBeaconHandler returns a mock beacon node handler. It registers a few mock handlers required by the
// eth2http service on startup, all other requests are routed to ProxyHandler if not nil.
func (h testHandler) newBeaconHandler(t *testing.T) http.Handler {
	t.Helper()

	ctx := context.Background()
	mock, err := eth2mock.New(ctx, eth2mock.WithLogLevel(infoLevel))
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/eth/v1/beacon/genesis", func(w http.ResponseWriter, r *http.Request) {
		res, err := mock.Genesis(ctx, &eth2api.GenesisOpts{})
		require.NoError(t, err)
		writeResponse(ctx, w, "", res.Data, nil)
	})
	mux.HandleFunc("/eth/v1/config/spec", func(w http.ResponseWriter, r *http.Request) {
		res := map[string]any{
			"SLOTS_PER_EPOCH":    strconv.Itoa(slotsPerEpoch),
			"ELECTRA_FORK_EPOCH": strconv.Itoa(electraForkEpoch),
		}
		writeResponse(ctx, w, "", nest(res, "data"), nil)
	})
	mux.HandleFunc("/eth/v1/config/deposit_contract", func(w http.ResponseWriter, r *http.Request) {
		res, err := mock.DepositContract(ctx, &eth2api.DepositContractOpts{})
		require.NoError(t, err)
		writeResponse(ctx, w, "", res.Data, nil)
	})
	mux.HandleFunc("/eth/v1/config/fork_schedule", func(w http.ResponseWriter, r *http.Request) {
		res, err := mock.ForkSchedule(ctx, &eth2api.ForkScheduleOpts{})
		require.NoError(t, err)
		writeResponse(ctx, w, "", nest(res.Data, "data"), nil)
	})
	mux.HandleFunc("/eth/v2/debug/beacon/states/head", func(w http.ResponseWriter, r *http.Request) {
		res := testutil.RandomBeaconState(t)
		w.Header().Add(versionHeader, res.Version.String())

		writeResponse(ctx, w, "", nest(res.Capella, "data"), nil)
	})
	mux.HandleFunc("/eth/v1/node/syncing", func(w http.ResponseWriter, r *http.Request) {
		writeResponse(ctx, w, "", nest(map[string]any{"is_syncing": false, "head_slot": "1", "sync_distance": "1"}, "data"), nil)
	})

	if h.ProxyHandler != nil {
		mux.HandleFunc("/", h.ProxyHandler)
	}

	return mux
}

// nest returns a json nested version the data objected. Note nests must be provided in inverse order.
func nest(data any, nests ...string) any {
	res := data

	for _, nest := range nests {
		res = map[string]interface{}{
			nest: res,
		}
	}

	return res
}

// testBeaconAddr implements eth2client.Service only returning an address.
type testBeaconAddr struct {
	eth2wrap.Client

	addr string
}

func (t testBeaconAddr) Address() string {
	return t.addr
}
