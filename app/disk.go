// Copyright © 2022-2025 Obol Labs Inc. Licensed under the terms of a Business Source License 1.1

package app

import (
	"context"
	"os"

	"github.com/obolnetwork/charon/app/errors"
	"github.com/obolnetwork/charon/app/eth1wrap"
	"github.com/obolnetwork/charon/app/log"
	"github.com/obolnetwork/charon/cluster"
	"github.com/obolnetwork/charon/cluster/manifest"
	manifestpb "github.com/obolnetwork/charon/cluster/manifestpb/v1"
)

// loadClusterManifest returns the cluster manifest from the given file path.
func loadClusterManifest(ctx context.Context, conf Config, eth1Cl eth1wrap.EthClientRunner) (*manifestpb.Cluster, error) {
	if conf.TestConfig.Lock != nil {
		return manifest.NewClusterFromLockForT(nil, *conf.TestConfig.Lock)
	}

	verifyLock := func(lock cluster.Lock) error {
		if conf.NoVerify {
			if err := lock.VerifyHashes(); err != nil {
				log.Warn(ctx, "Ignoring failed cluster lock hash verification due to --no-verify flag", err)
			}

			if err := lock.VerifySignatures(eth1Cl); err != nil {
				log.Warn(ctx, "Ignoring failed cluster lock signature verification due to --no-verify flag", err)
			}

			return nil
		}

		if err := lock.VerifyHashes(); err != nil {
			return errors.Wrap(err, "cluster lock hash verification failed. Run with --no-verify to bypass verification at own risk")
		}

		if err := lock.VerifySignatures(eth1Cl); err != nil {
			return errors.Wrap(err, "cluster lock signature verification failed. Run with --no-verify to bypass verification at own risk")
		}

		return nil
	}

	cluster, err := manifest.LoadCluster(conf.ManifestFile, conf.LockFile, verifyLock)
	if err != nil {
		return nil, errors.Wrap(err, "load cluster manifest")
	}

	return cluster, nil
}

// FileExists checks if a file exists at the given path.
func FileExists(path string) bool {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}

	return err == nil
}
