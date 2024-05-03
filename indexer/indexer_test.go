package indexer_test

import (
	"math/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/babylonchain/babylon/btcstaking"
	bbndatagen "github.com/babylonchain/babylon/testutil/datagen"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/babylonchain/staking-indexer/config"
	"github.com/babylonchain/staking-indexer/indexer"
	"github.com/babylonchain/staking-indexer/testutils"
	"github.com/babylonchain/staking-indexer/testutils/datagen"
	"github.com/babylonchain/staking-indexer/testutils/mocks"
	"github.com/babylonchain/staking-indexer/types"
)

type StakingTxData struct {
	StakingTx   *btcutil.Tx
	StakingData *datagen.TestStakingData
}

// FuzzIndexer tests the property that the indexer can correctly
// parse staking tx from confirmed blocks
func FuzzIndexer(f *testing.F) {
	// use small seed because db open/close is slow
	bbndatagen.AddRandomSeedsToFuzzer(f, 5)

	f.Fuzz(func(t *testing.T, seed int64) {
		r := rand.New(rand.NewSource(seed))

		homePath := filepath.Join(t.TempDir(), "indexer")
		cfg := config.DefaultConfigWithHome(homePath)

		confirmedBlockChan := make(chan *types.IndexedBlock)
		sysParamsVersions := datagen.GenerateGlobalParamsVersions(r, t)

		db, err := cfg.DatabaseConfig.GetDbBackend()
		require.NoError(t, err)
		mockBtcScanner := NewMockedBtcScanner(t, confirmedBlockChan)
		stakingIndexer, err := indexer.NewStakingIndexer(cfg, zap.NewNop(), NewMockedConsumer(t), db, sysParamsVersions, mockBtcScanner)
		require.NoError(t, err)

		err = stakingIndexer.Start(1)
		require.NoError(t, err)
		defer func() {
			err := stakingIndexer.Stop()
			require.NoError(t, err)
			err = db.Close()
			require.NoError(t, err)
		}()

		// 1. build staking tx and insert them into blocks
		// and send block to the confirmed block channel
		numBlocks := r.Intn(10) + 1
		// Starting height should be at least later than the activation height of the first parameters
		startingHeight := r.Int31n(1000) + 1 + sysParamsVersions.ParamsVersions[0].ActivationHeight

		stakingDataList := make([]*datagen.TestStakingData, 0)
		stakingTxList := make([]*btcutil.Tx, 0)
		unbondingTxList := make([]*btcutil.Tx, 0)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < numBlocks; i++ {
				blockHeight := startingHeight + int32(i)
				params, err := sysParamsVersions.GetParamsForBTCHeight(blockHeight)
				require.NoError(t, err)
				numTxs := r.Intn(10) + 1
				blockTxs := make([]*btcutil.Tx, 0)
				for j := 0; j < numTxs; j++ {
					stakingData := datagen.GenerateTestStakingData(t, r, params)
					stakingDataList = append(stakingDataList, stakingData)
					_, stakingTx := datagen.GenerateStakingTxFromTestData(t, r, params, stakingData)
					unbondingTx := datagen.GenerateUnbondingTxFromStaking(t, params, stakingData, stakingTx.Hash(), 0)
					blockTxs = append(blockTxs, stakingTx)
					blockTxs = append(blockTxs, unbondingTx)
					stakingTxList = append(stakingTxList, stakingTx)
					unbondingTxList = append(unbondingTxList, unbondingTx)
				}
				b := &types.IndexedBlock{
					Height: blockHeight,
					Txs:    blockTxs,
					Header: &wire.BlockHeader{Timestamp: time.Now()},
				}
				confirmedBlockChan <- b
			}
		}()
		wg.Wait()

		// wait for db writes finished
		time.Sleep(2 * time.Second)

		// 2. read local store and expect them to be the
		// same as the data before being stored
		for i := 0; i < len(stakingTxList); i++ {
			tx := stakingTxList[i].MsgTx()
			txHash := tx.TxHash()
			data := stakingDataList[i]
			storedTx, err := stakingIndexer.GetStakingTxByHash(&txHash)
			require.NoError(t, err)
			require.Equal(t, tx.TxHash(), storedTx.Tx.TxHash())
			require.True(t, testutils.PubKeysEqual(data.StakerKey, storedTx.StakerPk))
			require.Equal(t, uint32(data.StakingTime), storedTx.StakingTime)
			require.True(t, testutils.PubKeysEqual(data.FinalityProviderKey, storedTx.FinalityProviderPk))
		}

		for i := 0; i < len(unbondingTxList); i++ {
			tx := unbondingTxList[i].MsgTx()
			txHash := tx.TxHash()
			expectedStakingTx := stakingTxList[i].MsgTx()
			storedUnbondingTx, err := stakingIndexer.GetUnbondingTxByHash(&txHash)
			require.NoError(t, err)
			require.Equal(t, tx.TxHash(), storedUnbondingTx.Tx.TxHash())
			require.Equal(t, expectedStakingTx.TxHash().String(), storedUnbondingTx.StakingTxHash.String())

			expectedStakingData := stakingDataList[i]
			storedStakingTx, err := stakingIndexer.GetStakingTxByHash(storedUnbondingTx.StakingTxHash)
			require.NoError(t, err)
			require.Equal(t, expectedStakingTx.TxHash(), storedStakingTx.Tx.TxHash())
			require.True(t, testutils.PubKeysEqual(expectedStakingData.StakerKey, storedStakingTx.StakerPk))
			require.Equal(t, uint32(expectedStakingData.StakingTime), storedStakingTx.StakingTime)
			require.True(t, testutils.PubKeysEqual(expectedStakingData.FinalityProviderKey, storedStakingTx.FinalityProviderPk))
		}
	})
}

// FuzzVerifyUnbondingTx tests IsValidUnbondingTx in three scenarios:
// 1. it returns (true, nil) if the given tx is valid unbonding tx
// 2. it returns (false, nil) if the given tx is not unbonding tx
// 3. it returns (false, ErrInvalidUnbondingTx) if the given tx is not
// a valid unbonding tx
func FuzzVerifyUnbondingTx(f *testing.F) {
	bbndatagen.AddRandomSeedsToFuzzer(f, 50)

	f.Fuzz(func(t *testing.T, seed int64) {
		r := rand.New(rand.NewSource(seed))

		homePath := filepath.Join(t.TempDir(), "indexer")
		cfg := config.DefaultConfigWithHome(homePath)

		confirmedBlockChan := make(chan *types.IndexedBlock)
		sysParamsVersions := datagen.GenerateGlobalParamsVersions(r, t)

		db, err := cfg.DatabaseConfig.GetDbBackend()
		require.NoError(t, err)
		mockBtcScanner := NewMockedBtcScanner(t, confirmedBlockChan)
		stakingIndexer, err := indexer.NewStakingIndexer(cfg, zap.NewNop(), NewMockedConsumer(t), db, sysParamsVersions, mockBtcScanner)
		require.NoError(t, err)
		defer func() {
			err = db.Close()
			require.NoError(t, err)
		}()

		// Select the first params versions to play with
		params := sysParamsVersions.ParamsVersions[0]
		// 1. generate and add a valid staking tx to the indexer
		stakingData := datagen.GenerateTestStakingData(t, r, params)
		_, stakingTx := datagen.GenerateStakingTxFromTestData(t, r, params, stakingData)
		// For a valid tx, its btc height is always larger than the activation height
		mockedHeight := uint64(params.ActivationHeight) + 1
		err = stakingIndexer.ProcessStakingTx(
			stakingTx.MsgTx(),
			getParsedStakingData(stakingData, stakingTx.MsgTx(), params),
			mockedHeight, time.Now())
		require.NoError(t, err)
		storedStakingTx, err := stakingIndexer.GetStakingTxByHash(stakingTx.Hash())
		require.NoError(t, err)

		// 2. test IsValidUnbondingTx with valid unbonding tx, expect (true, nil)
		unbondingTx := datagen.GenerateUnbondingTxFromStaking(t, params, stakingData, stakingTx.Hash(), 0)
		isValid, err := stakingIndexer.IsValidUnbondingTx(unbondingTx.MsgTx(), storedStakingTx, params)
		require.NoError(t, err)
		require.True(t, isValid)

		// 3. test IsValidUnbondingTx with no unbonding tx (different staking output index), expect (false, nil)
		unbondingTx = datagen.GenerateUnbondingTxFromStaking(t, params, stakingData, stakingTx.Hash(), 1)
		isValid, err = stakingIndexer.IsValidUnbondingTx(unbondingTx.MsgTx(), storedStakingTx, params)
		require.NoError(t, err)
		require.False(t, isValid)

		// 4. test IsValidUnbondingTx with invalid unbonding tx (random unbonding fee in params), expect (false, ErrInvalidUnbondingTx)
		newParams := *params
		newParams.UnbondingFee = btcutil.Amount(bbndatagen.RandomIntOtherThan(r, int(params.UnbondingFee), 10000000))
		unbondingTx = datagen.GenerateUnbondingTxFromStaking(t, &newParams, stakingData, stakingTx.Hash(), 0)
		// pass the old params
		isValid, err = stakingIndexer.IsValidUnbondingTx(unbondingTx.MsgTx(), storedStakingTx, params)
		require.ErrorIs(t, err, indexer.ErrInvalidUnbondingTx)
		require.False(t, isValid)
	})
}

// The eligibility status of a staking tx is determined by the following rules:
// 1. If staking cap will be exceeded after the staking tx, the tx is ineligible
// 2. If the staking tx is not in the range of staking min and max stake values, the tx is ineligible
// 3. If the staking tx is not in the range of staking min and max time values, the tx is ineligible
// This test covers the staking cap rule only
func FuzzCalculateStakingEligibilityStatusBasedOnStakingCap(f *testing.F) {
	bbndatagen.AddRandomSeedsToFuzzer(f, 5)

	f.Fuzz(func(t *testing.T, seed int64) {
		r := rand.New(rand.NewSource(seed))

		homePath := filepath.Join(t.TempDir(), "indexer")
		cfg := config.DefaultConfigWithHome(homePath)

		confirmedBlockChan := make(chan *types.IndexedBlock)
		sysParamsVersions := datagen.GenerateGlobalParamsVersions(r, t)

		db, err := cfg.DatabaseConfig.GetDbBackend()
		require.NoError(t, err)
		mockBtcScanner := NewMockedBtcScanner(t, confirmedBlockChan)
		stakingIndexer, err := indexer.NewStakingIndexer(cfg, zap.NewNop(), NewMockedConsumer(t), db, sysParamsVersions, mockBtcScanner)
		require.NoError(t, err)
		defer func() {
			err = db.Close()
			require.NoError(t, err)
		}()

		// Select the first params versions to play with
		params := sysParamsVersions.ParamsVersions[0]

		remainingStakingCap := params.StakingCap

		// Accumulate the test data
		var stakingTxData []StakingTxData
		// Keep sending staking tx until the staking cap is exceeded
		for {
			stakingData := datagen.GenerateTestStakingData(t, r, params)
			_, stakingTx := datagen.GenerateStakingTxFromTestData(t, r, params, stakingData)
			// For a valid tx, its btc height is always larger than the activation height
			mockedHeight := uint64(params.ActivationHeight) + 1
			err = stakingIndexer.ProcessStakingTx(
				stakingTx.MsgTx(),
				getParsedStakingData(stakingData, stakingTx.MsgTx(), params),
				mockedHeight, time.Now())
			require.NoError(t, err)
			storedStakingTx, err := stakingIndexer.GetStakingTxByHash(stakingTx.Hash())
			require.NoError(t, err)

			stakingTxData = append(stakingTxData, StakingTxData{
				StakingTx:   stakingTx,
				StakingData: stakingData,
			})
			remainingStakingCap -= btcutil.Amount(storedStakingTx.StakingValue)
			if remainingStakingCap < 0 {
				break
			}
		}

		// Check the eligibility status of each staking tx
		for index, data := range stakingTxData {
			storedStakingTx, err := stakingIndexer.GetStakingTxByHash(data.StakingTx.Hash())
			require.NoError(t, err)
			// check all staking txs are eligible
			if index < len(stakingTxData)-1 {
				require.Equal(t, types.EligibilityStatusActive, storedStakingTx.EligibilityStatus)
			} else {
				require.Equal(t, types.EligibilityStatusInactive, storedStakingTx.EligibilityStatus)
			}
		}

		// Now let's unbond some of the tx so that the TVL is below the staking cap
		// and check the eligibility status of each staking tx
		for _, data := range stakingTxData {
			storedStakingTx, err := stakingIndexer.GetStakingTxByHash(data.StakingTx.Hash())
			require.NoError(t, err)
			// unbond the staking tx
			unbondingTx := datagen.GenerateUnbondingTxFromStaking(t, params, data.StakingData, data.StakingTx.Hash(), 0)
			mockedHeight := uint64(params.ActivationHeight) + 2
			err = stakingIndexer.ProcessUnbondingTx(
				unbondingTx.MsgTx(),
				data.StakingTx.Hash(),
				mockedHeight, time.Now(), params)
			require.NoError(t, err)
			_, err = stakingIndexer.GetUnbondingTxByHash(unbondingTx.Hash())
			require.NoError(t, err)

			// We revert the calculation
			remainingStakingCap += btcutil.Amount(storedStakingTx.StakingValue)

			// Let's break if current tvl is already below the staking cap (i.e remainingStakingCap > 0)
			if remainingStakingCap > 0 {
				break
			}
		}

		// let's send more staking txs until the staking cap is exceeded again
		var stakingTxData2 []StakingTxData
		for {
			stakingData := datagen.GenerateTestStakingData(t, r, params)
			_, stakingTx := datagen.GenerateStakingTxFromTestData(t, r, params, stakingData)
			// For a valid tx, its btc height is always larger than the activation height
			mockedHeight := uint64(params.ActivationHeight) + 3
			err = stakingIndexer.ProcessStakingTx(
				stakingTx.MsgTx(),
				getParsedStakingData(stakingData, stakingTx.MsgTx(), params),
				mockedHeight, time.Now())
			require.NoError(t, err)
			storedStakingTx, err := stakingIndexer.GetStakingTxByHash(stakingTx.Hash())
			require.NoError(t, err)

			stakingTxData2 = append(stakingTxData, StakingTxData{
				StakingTx:   stakingTx,
				StakingData: stakingData,
			})
			remainingStakingCap -= btcutil.Amount(storedStakingTx.StakingValue)
			if remainingStakingCap < 0 {
				break
			}
		}

		// Check the eligibility status of each staking tx from the second batch
		for index, data := range stakingTxData2 {
			storedStakingTx, err := stakingIndexer.GetStakingTxByHash(data.StakingTx.Hash())
			require.NoError(t, err)
			// check all staking txs are eligible
			if index < len(stakingTxData2)-1 {
				require.Equal(t, types.EligibilityStatusActive, storedStakingTx.EligibilityStatus)
			} else {
				require.Equal(t, types.EligibilityStatusInactive, storedStakingTx.EligibilityStatus)
			}
		}

		// Now let's use the second params
		paramsSecond := sysParamsVersions.ParamsVersions[1]

		// find the staking cap gap
		stakingCapGap := paramsSecond.StakingCap - params.StakingCap

		remainingStakingCap = stakingCapGap + remainingStakingCap
		// TODO: Continue the calculation based on the second params

	})
}

func getParsedStakingData(data *datagen.TestStakingData, tx *wire.MsgTx, params *types.Params) *btcstaking.ParsedV0StakingTx {
	return &btcstaking.ParsedV0StakingTx{
		StakingOutput:     tx.TxOut[0],
		StakingOutputIdx:  0,
		OpReturnOutput:    tx.TxOut[1],
		OpReturnOutputIdx: 1,
		OpReturnData: &btcstaking.V0OpReturnData{
			MagicBytes:                params.Tag,
			Version:                   0,
			StakerPublicKey:           &btcstaking.XonlyPubKey{PubKey: data.StakerKey},
			FinalityProviderPublicKey: &btcstaking.XonlyPubKey{PubKey: data.FinalityProviderKey},
			StakingTime:               data.StakingTime,
		},
	}
}

func NewMockedConsumer(t *testing.T) *mocks.MockEventConsumer {
	ctl := gomock.NewController(t)
	mockedConsumer := mocks.NewMockEventConsumer(ctl)
	mockedConsumer.EXPECT().PushStakingEvent(gomock.Any()).Return(nil).AnyTimes()
	mockedConsumer.EXPECT().PushUnbondingEvent(gomock.Any()).Return(nil).AnyTimes()
	mockedConsumer.EXPECT().PushWithdrawEvent(gomock.Any()).Return(nil).AnyTimes()
	mockedConsumer.EXPECT().Start().Return(nil).AnyTimes()
	mockedConsumer.EXPECT().Stop().Return(nil).AnyTimes()

	return mockedConsumer
}

func NewMockedBtcScanner(t *testing.T, confirmedBlocksChan chan *types.IndexedBlock) *mocks.MockBtcScanner {
	ctl := gomock.NewController(t)
	mockBtcScanner := mocks.NewMockBtcScanner(ctl)
	mockBtcScanner.EXPECT().Start(gomock.Any()).Return(nil).AnyTimes()
	mockBtcScanner.EXPECT().ConfirmedBlocksChan().Return(confirmedBlocksChan).AnyTimes()
	mockBtcScanner.EXPECT().Stop().Return(nil).AnyTimes()

	return mockBtcScanner
}
