// Copyright © 2022-2025 Obol Labs Inc. Licensed under the terms of a Business Source License 1.1

package deposit_test

import (
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"testing"

	eth2p0 "github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/stretchr/testify/require"

	"github.com/obolnetwork/charon/eth2util"
	"github.com/obolnetwork/charon/eth2util/deposit"
	"github.com/obolnetwork/charon/tbls"
	"github.com/obolnetwork/charon/tbls/tblsconv"
	"github.com/obolnetwork/charon/testutil"
)

//go:generate go test . -run=TestMarshalDepositData -update -clean

func TestNewMessage(t *testing.T) {
	const (
		privKey = "01477d4bfbbcebe1fef8d4d6f624ecbb6e3178558bb1b0d6286c816c66842a6d"
		addr    = "0x321dcb529f3945bc94fecea9d3bc5caf35253b94"
	)

	amount := deposit.DefaultDepositAmount
	_, pubKey := GetKeys(t, privKey)

	msg, err := deposit.NewMessage(pubKey, addr, amount, false)

	require.NoError(t, err)
	require.Equal(t, pubKey, msg.PublicKey)
	require.Equal(t, amount, msg.Amount)

	t.Run("amount below minimum", func(t *testing.T) {
		_, err := deposit.NewMessage(pubKey, addr, deposit.MinDepositAmount-1, false)

		require.ErrorContains(t, err, "deposit message minimum amount must be >= 1ETH")
	})

	tests := []struct {
		compouding bool
		max        eth2p0.Gwei
	}{
		{false, deposit.MaxStandardDepositAmount},
		{true, deposit.MaxCompoundingDepositAmount},
	}

	for _, test := range tests {
		t.Run("amount above maximum", func(t *testing.T) {
			_, err := deposit.NewMessage(pubKey, addr, test.max+1, test.compouding)

			require.ErrorContains(t, err, "deposit message maximum amount exceeded")
		})

		t.Run("valid max amount", func(t *testing.T) {
			msg, err := deposit.NewMessage(pubKey, addr, test.max, test.compouding)

			require.NoError(t, err)

			if test.compouding {
				require.Equal(t, []byte{0x02}, msg.WithdrawalCredentials[:1])
			} else {
				require.Equal(t, []byte{0x01}, msg.WithdrawalCredentials[:1])
			}
		})
	}
}

func TestMarshalDepositData(t *testing.T) {
	datas := mustGenerateDepositDatas(t, deposit.DefaultDepositAmount)

	actual, err := deposit.MarshalDepositData(datas, eth2util.Goerli.Name)
	require.NoError(t, err)

	testutil.RequireGoldenBytes(t, actual)
}

// Get the private and public keys in appropriate format for the test.
func GetKeys(t *testing.T, privKey string) (tbls.PrivateKey, eth2p0.BLSPubKey) {
	t.Helper()

	privKeyBytes, err := hex.DecodeString(privKey)
	require.NoError(t, err)

	sk, err := tblsconv.PrivkeyFromBytes(privKeyBytes)
	require.NoError(t, err)

	pk, err := tbls.SecretToPublicKey(sk)
	require.NoError(t, err)

	pubkey, err := tblsconv.PubkeyToETH2(pk)
	require.NoError(t, err)

	return sk, pubkey
}

func TestVerifyDepositAmounts(t *testing.T) {
	t.Run("empty slice", func(t *testing.T) {
		err := deposit.VerifyDepositAmounts(nil, false)

		require.NoError(t, err)
	})

	t.Run("valid amounts", func(t *testing.T) {
		amounts := []eth2p0.Gwei{
			eth2p0.Gwei(16000000000),
			eth2p0.Gwei(16000000000),
		}

		err := deposit.VerifyDepositAmounts(amounts, false)

		require.NoError(t, err)
	})

	t.Run("each amount is greater than 1ETH", func(t *testing.T) {
		amounts := []eth2p0.Gwei{
			eth2p0.Gwei(500000000),   // 0.5ETH
			eth2p0.Gwei(31500000000), // 31.5ETH
		}

		err := deposit.VerifyDepositAmounts(amounts, false)

		require.ErrorContains(t, err, "each partial deposit amount must be greater than 1ETH")
	})

	t.Run("each amount is lesser than max", func(t *testing.T) {
		amounts := []eth2p0.Gwei{
			deposit.MinDepositAmount,                                // 1ETH
			deposit.DefaultDepositAmount + deposit.MinDepositAmount, // 33ETH
		}

		err := deposit.VerifyDepositAmounts(amounts, false)
		require.ErrorContains(t, err, "single partial deposit amount is too large unless --compounding validators are used")

		err = deposit.VerifyDepositAmounts(amounts, true)
		require.NoError(t, err)

		amounts = append(amounts, deposit.MaxCompoundingDepositAmount+deposit.MinDepositAmount) // 2049ETH

		err = deposit.VerifyDepositAmounts(amounts, true)
		require.ErrorContains(t, err, "single partial deposit amount is too large unless --compounding validators are used")
	})

	t.Run("total sum is at least 32ETH", func(t *testing.T) {
		amounts := []eth2p0.Gwei{
			eth2p0.Gwei(8000000000),
			eth2p0.Gwei(16000000000),
		}

		err := deposit.VerifyDepositAmounts(amounts, false)

		require.ErrorContains(t, err, "sum of partial deposit amounts must be at least 32ETH, repetition is allowed")
	})
}

func TestDefaultDepositAmounts(t *testing.T) {
	amounts := deposit.DefaultDepositAmounts(false)

	require.Equal(t, []eth2p0.Gwei{
		deposit.MinDepositAmount,
		deposit.DefaultDepositAmount,
	}, amounts)

	amounts = deposit.DefaultDepositAmounts(true)

	require.Equal(t, []eth2p0.Gwei{
		deposit.MinDepositAmount,
		8 * deposit.OneEthInGwei,
		32 * deposit.OneEthInGwei,
		256 * deposit.OneEthInGwei,
	}, amounts)
}

func TestEthsToGweis(t *testing.T) {
	t.Run("nil slice", func(t *testing.T) {
		slice := deposit.EthsToGweis(nil)

		require.Nil(t, slice)
	})

	t.Run("values", func(t *testing.T) {
		slice := deposit.EthsToGweis([]int{1, 5})

		require.Equal(t, []eth2p0.Gwei{
			eth2p0.Gwei(1000000000),
			eth2p0.Gwei(5000000000),
		}, slice)
	})
}

func TestGetDepositFilePath(t *testing.T) {
	dir := t.TempDir()

	filepath := deposit.GetDepositFilePath(dir, deposit.MinDepositAmount)
	require.Equal(t, path.Join(dir, "deposit-data-1eth.json"), filepath)

	filepath = deposit.GetDepositFilePath(dir, deposit.DefaultDepositAmount-1)
	require.Equal(t, path.Join(dir, "deposit-data-31.999999999eth.json"), filepath)

	filepath = deposit.GetDepositFilePath(dir, deposit.DefaultDepositAmount)
	require.Equal(t, path.Join(dir, "deposit-data.json"), filepath)
}

func TestWriteDepositDataFile(t *testing.T) {
	dir := t.TempDir()
	depositDatas := mustGenerateDepositDatas(t, deposit.DefaultDepositAmount)

	err := deposit.WriteDepositDataFile(depositDatas, eth2util.Goerli.Name, dir)
	require.NoError(t, err)

	expected, err := deposit.MarshalDepositData(depositDatas, eth2util.Goerli.Name)
	require.NoError(t, err)

	filepath := deposit.GetDepositFilePath(dir, deposit.DefaultDepositAmount)
	actual, err := os.ReadFile(filepath)

	require.NoError(t, err)
	require.Equal(t, expected, actual)

	t.Run("empty deposit datas", func(t *testing.T) {
		err := deposit.WriteDepositDataFile([]eth2p0.DepositData{}, eth2util.Goerli.Name, dir)
		require.ErrorContains(t, err, "empty deposit data")
	})

	t.Run("not equal amounts", func(t *testing.T) {
		depositDatas[1].Amount /= 2
		err := deposit.WriteDepositDataFile(depositDatas, eth2util.Goerli.Name, dir)
		require.ErrorContains(t, err, "deposit datas has different amount")
	})
}

func TestWriteClusterDepositDataFiles(t *testing.T) {
	const numNodes = 4

	dir := t.TempDir()

	for n := range numNodes {
		err := os.MkdirAll(path.Join(dir, fmt.Sprintf("node%d", n)), 0o755)
		require.NoError(t, err)
	}

	var depositDatas [][]eth2p0.DepositData

	depositDatas = append(depositDatas, mustGenerateDepositDatas(t, deposit.DefaultDepositAmount/2))
	depositDatas = append(depositDatas, mustGenerateDepositDatas(t, deposit.DefaultDepositAmount/4))

	err := deposit.WriteClusterDepositDataFiles(depositDatas, eth2util.Goerli.Name, dir, numNodes)
	require.NoError(t, err)

	for i := range depositDatas {
		expected, err := deposit.MarshalDepositData(depositDatas[i], eth2util.Goerli.Name)
		require.NoError(t, err)

		for n := range numNodes {
			nodeDir := path.Join(dir, fmt.Sprintf("node%d", n))
			filepath := deposit.GetDepositFilePath(nodeDir, depositDatas[i][0].Amount)
			actual, err := os.ReadFile(filepath)

			require.NoError(t, err)
			require.Equal(t, expected, actual)
		}
	}
}

func mustGenerateDepositDatas(t *testing.T, amount eth2p0.Gwei) []eth2p0.DepositData {
	t.Helper()

	privKeys := []string{
		"01477d4bfbbcebe1fef8d4d6f624ecbb6e3178558bb1b0d6286c816c66842a6d",
		"5b77c0f0ef7c4ddc123d55b8bd93daeefbd7116764a941c0061a496649e145b5",
		"1dabcbfc9258f0f28606bf9e3b1c9f06d15a6e4eb0fbc28a43835eaaed7623fc",
		"002ff4fd29d3deb6de9f5d115182a49c618c97acaa365ad66a0b240bd825c4ff",
	}
	withdrawalAddrs := []string{
		"0x321dcb529f3945bc94fecea9d3bc5caf35253b94",
		"0x08ef6a66a4f315aa250d2e748de0bfe5a6121096",
		"0x05f9f73f74c205f2b9267c04296e3069767531fb",
		"0x67f5df029ae8d3f941abef0bec6462a6b4e4b522",
	}

	var (
		datas   []eth2p0.DepositData
		network = eth2util.Goerli.Name
	)

	for i := range len(privKeys) {
		sk, pk := GetKeys(t, privKeys[i])

		msg, err := deposit.NewMessage(pk, withdrawalAddrs[i], amount, true)
		require.NoError(t, err)

		sigRoot, err := deposit.GetMessageSigningRoot(msg, network)
		require.NoError(t, err)

		sig, err := tbls.Sign(sk, sigRoot[:])
		require.NoError(t, err)

		datas = append(datas, eth2p0.DepositData{
			PublicKey:             msg.PublicKey,
			WithdrawalCredentials: msg.WithdrawalCredentials,
			Amount:                msg.Amount,
			Signature:             tblsconv.SigToETH2(sig),
		})
	}

	return datas
}

func TestDedupAmounts(t *testing.T) {
	amounts := []eth2p0.Gwei{100, 500, 100, 0, 0, 300}

	amounts = deposit.DedupAmounts(amounts)

	require.Equal(t, []eth2p0.Gwei{0, 100, 300, 500}, amounts)
}
