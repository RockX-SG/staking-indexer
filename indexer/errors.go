package indexer

import "errors"

var (
	// ErrInvalidGlobalParameters the global parameters are invalid
	ErrInvalidGlobalParameters = errors.New("invalid parameters")

	// ErrInvalidUnbondingTx the transaction spends the unbonding path but is invalid
	ErrInvalidUnbondingTx = errors.New("invalid unbonding tx")

	// ErrInvalidStakingTx the stake transaction is invalid as it does not follow the global parameters
	ErrInvalidStakingTx = errors.New("invalid staking tx")
)
