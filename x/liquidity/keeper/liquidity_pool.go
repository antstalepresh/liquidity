package keeper

import (
	"fmt"
	"strconv"

	sdkerrors "cosmossdk.io/errors"
	"cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"

	"github.com/Victor118/liquidity/x/liquidity/types"
)

func (k Keeper) ValidateMsgCreatePool(ctx sdk.Context, msg *types.MsgCreatePool) error {
	params := k.GetParams(ctx)
	var poolType types.PoolType

	// check creator has permission to create pool
	if !isWhitelistedAddress(msg.PoolCreatorAddress, params) {
		return types.ErrNotPermissonedCreator
	}

	// check poolType exist, get poolType from param
	if len(params.PoolTypes) >= int(msg.PoolTypeId) {
		poolType = params.PoolTypes[msg.PoolTypeId-1]
		if poolType.Id != msg.PoolTypeId {
			return types.ErrPoolTypeNotExists
		}
	} else {
		return types.ErrPoolTypeNotExists
	}

	reserveCoinNum := uint32(msg.DepositCoins.Len())
	if reserveCoinNum > poolType.MaxReserveCoinNum || poolType.MinReserveCoinNum > reserveCoinNum {
		return types.ErrNumOfReserveCoin
	}

	reserveCoinDenoms := make([]string, reserveCoinNum)
	for i := 0; i < int(reserveCoinNum); i++ {
		reserveCoinDenoms[i] = msg.DepositCoins.GetDenomByIndex(i)
	}

	denomA, denomB := types.AlphabeticalDenomPair(reserveCoinDenoms[0], reserveCoinDenoms[1])
	if denomA != msg.DepositCoins[0].Denom || denomB != msg.DepositCoins[1].Denom {
		return types.ErrBadOrderingReserveCoin
	}

	if denomA == denomB {
		return types.ErrEqualDenom
	}

	if err := types.ValidateReserveCoinLimit(params.MaxReserveCoinAmount, msg.DepositCoins); err != nil {
		return err
	}

	poolName := types.PoolName(reserveCoinDenoms, msg.PoolTypeId)
	reserveAcc := types.GetPoolReserveAcc(poolName)
	_, found := k.GetPoolByReserveAccIndex(ctx, reserveAcc)
	if found {
		return types.ErrPoolAlreadyExists
	}
	return nil
}

func (k Keeper) MintAndSendPoolCoin(ctx sdk.Context, pool types.Pool, srcAddr, creatorAddr sdk.AccAddress, depositCoins sdk.Coins) (sdk.Coin, error) {
	cacheCtx, writeCache := ctx.CacheContext()

	params := k.GetParams(cacheCtx)

	mintingCoin := sdk.NewCoin(pool.PoolCoinDenom, params.InitPoolCoinMintAmount)
	mintingCoins := sdk.NewCoins(mintingCoin)
	if err := k.bankKeeper.MintCoins(cacheCtx, types.ModuleName, mintingCoins); err != nil {
		return sdk.Coin{}, err
	}

	reserveAcc := pool.GetReserveAccount()

	var input banktypes.Input
	var outputs []banktypes.Output

	input = banktypes.NewInput(srcAddr, depositCoins)
	outputs = append(outputs, banktypes.NewOutput(reserveAcc, depositCoins))

	if err := k.bankKeeper.InputOutputCoins(cacheCtx, input, outputs); err != nil {
		return sdk.Coin{}, err
	}

	input = banktypes.NewInput(k.accountKeeper.GetModuleAddress(types.ModuleName), mintingCoins)
	outputs = append([]banktypes.Output{}, banktypes.NewOutput(creatorAddr, mintingCoins))

	if err := k.bankKeeper.InputOutputCoins(cacheCtx, input, outputs); err != nil {
		return sdk.Coin{}, err
	}

	writeCache()

	return mintingCoin, nil
}

func (k Keeper) CreatePool(ctx sdk.Context, msg *types.MsgCreatePool) (types.Pool, error) {
	if err := k.ValidateMsgCreatePool(ctx, msg); err != nil {
		return types.Pool{}, err
	}

	sendCoin := func(from, to sdk.AccAddress, coin sdk.Coin) error {
		coins := sdk.NewCoins(coin)

		if !coins.Empty() && coins.IsValid() {
			var input banktypes.Input
			var outputs []banktypes.Output
			input = banktypes.NewInput(from, coins)
			outputs = append(outputs, banktypes.NewOutput(to, coins))
			if err := k.bankKeeper.InputOutputCoins(ctx, input, outputs); err != nil {
				return err
			}
		}
		return nil
	}

	params := k.GetParams(ctx)

	denom1, denom2 := types.AlphabeticalDenomPair(msg.DepositCoins[0].Denom, msg.DepositCoins[1].Denom)
	reserveCoinDenoms := []string{denom1, denom2}

	poolName := types.PoolName(reserveCoinDenoms, msg.PoolTypeId)

	pool := types.Pool{
		//Id: will set on SetPoolAtomic
		TypeId:                msg.PoolTypeId,
		ReserveCoinDenoms:     reserveCoinDenoms,
		ReserveAccountAddress: types.GetPoolReserveAcc(poolName).String(),
		PoolCoinDenom:         types.GetPoolCoinDenom(poolName),
	}

	err := types.CreateModuleAccount(ctx, k.accountKeeper, types.GetPoolReserveAcc(poolName))
	if err != nil {
		return types.Pool{}, err
	}

	poolCreator := msg.GetPoolCreator()

	for _, coin := range msg.DepositCoins {
		if coin.Amount.LT(params.MinInitDepositAmount) {
			return types.Pool{}, sdkerrors.Wrapf(
				types.ErrLessThanMinInitDeposit, "deposit coin %s is smaller than %s", coin, params.MinInitDepositAmount)
		}
	}

	for _, coin := range msg.DepositCoins {
		balance := k.bankKeeper.GetBalance(ctx, poolCreator, coin.Denom)
		if balance.IsLT(coin) {
			return types.Pool{}, sdkerrors.Wrapf(
				types.ErrInsufficientBalance, "%s is smaller than %s", balance, coin)
		}
	}

	communityFees := sdk.NewCoins()

	for _, coin := range params.PoolCreationFee {

		balance := k.bankKeeper.GetBalance(ctx, poolCreator, coin.Denom)
		neededAmt := coin.Amount.Add(msg.DepositCoins.AmountOf(coin.Denom))
		neededCoin := sdk.NewCoin(coin.Denom, neededAmt)
		if balance.IsLT(neededCoin) {
			return types.Pool{}, sdkerrors.Wrapf(
				types.ErrInsufficientPoolCreationFee, "%s is smaller than %s", balance, neededCoin)
		}
		if params.BuildersAddresses != nil && len(params.BuildersAddresses) > 0 {
			decCoin := sdk.NewDecCoinFromCoin(coin)
			builderComm := decCoin.Amount.Mul(params.BuildersCommission)
			buildersFeeCoin := sdk.NewCoin(coin.Denom, builderComm.TruncateInt())
			remainingCoin, err := k.SendAmountToBuilders(params, poolCreator, buildersFeeCoin, sendCoin)
			if err != nil {
				return types.Pool{}, err
			}
			buildersFeeCoin = buildersFeeCoin.Sub(remainingCoin)
			communityFees = communityFees.Add(coin.Sub(buildersFeeCoin))
		} else {
			communityFees = communityFees.Add(coin)
		}

	}

	if _, err := k.MintAndSendPoolCoin(ctx, pool, poolCreator, poolCreator, msg.DepositCoins); err != nil {
		return types.Pool{}, err
	}
	// pool creation fees are collected in community pool
	if err := k.distrKeeper.FundCommunityPool(ctx, communityFees, poolCreator); err != nil {
		return types.Pool{}, err
	}

	pool = k.SetPoolAtomic(ctx, pool)
	batch := types.NewPoolBatch(pool.Id, 1)
	batch.BeginHeight = ctx.BlockHeight()

	k.SetPoolBatch(ctx, batch)

	reserveCoins := k.GetReserveCoins(ctx, pool)
	lastReserveRatio := math.LegacyNewDecFromInt(reserveCoins[0].Amount).Quo(math.LegacyNewDecFromInt(reserveCoins[1].Amount))
	logger := k.Logger(ctx)
	logger.Debug(
		"create liquidity pool",
		"msg", msg,
		"pool", pool,
		"reserveCoins", reserveCoins,
		"lastReserveRatio", lastReserveRatio,
	)

	return pool, nil
}

func (k Keeper) ExecuteDeposit(ctx sdk.Context, msg types.DepositMsgState, batch types.PoolBatch) error {
	if msg.Executed || msg.ToBeDeleted || msg.Succeeded {
		return fmt.Errorf("cannot process already executed batch msg")
	}
	msg.Executed = true
	k.SetPoolBatchDepositMsgState(ctx, msg.Msg.PoolId, msg)

	if err := k.ValidateMsgDepositWithinBatch(ctx, *msg.Msg); err != nil {
		return err
	}

	pool, found := k.GetPool(ctx, msg.Msg.PoolId)
	if !found {
		return types.ErrPoolNotExists
	}

	depositCoins := msg.Msg.DepositCoins.Sort()

	batchEscrowAcc := k.accountKeeper.GetModuleAddress(types.ModuleName)
	reserveAcc := pool.GetReserveAccount()
	depositor := msg.Msg.GetDepositor()

	params := k.GetParams(ctx)

	reserveCoins := k.GetReserveCoins(ctx, pool)

	// reinitialize pool if the pool is depleted
	if k.IsDepletedPool(ctx, pool) {
		for _, depositCoin := range msg.Msg.DepositCoins {
			if depositCoin.Amount.Add(reserveCoins.AmountOf(depositCoin.Denom)).LT(params.MinInitDepositAmount) {
				return types.ErrLessThanMinInitDeposit
			}
		}
		poolCoin, err := k.MintAndSendPoolCoin(ctx, pool, batchEscrowAcc, depositor, msg.Msg.DepositCoins)
		if err != nil {
			return err
		}

		// set deposit msg state of the pool batch complete
		msg.Succeeded = true
		msg.ToBeDeleted = true
		k.SetPoolBatchDepositMsgState(ctx, msg.Msg.PoolId, msg)

		reserveCoins = k.GetReserveCoins(ctx, pool)
		lastReserveCoinA := math.LegacyNewDecFromInt(reserveCoins[0].Amount)
		lastReserveCoinB := math.LegacyNewDecFromInt(reserveCoins[1].Amount)
		lastReserveRatio := lastReserveCoinA.Quo(lastReserveCoinB)
		ctx.EventManager().EmitEvent(
			sdk.NewEvent(
				types.EventTypeDepositToPool,
				sdk.NewAttribute(types.AttributeValuePoolId, strconv.FormatUint(pool.Id, 10)),
				sdk.NewAttribute(types.AttributeValueBatchIndex, strconv.FormatUint(batch.Index, 10)),
				sdk.NewAttribute(types.AttributeValueMsgIndex, strconv.FormatUint(msg.MsgIndex, 10)),
				sdk.NewAttribute(types.AttributeValueDepositor, depositor.String()),
				sdk.NewAttribute(types.AttributeValueAcceptedCoins, msg.Msg.DepositCoins.String()),
				sdk.NewAttribute(types.AttributeValueRefundedCoins, ""),
				sdk.NewAttribute(types.AttributeValuePoolCoinDenom, poolCoin.Denom),
				sdk.NewAttribute(types.AttributeValuePoolCoinAmount, poolCoin.Amount.String()),
				sdk.NewAttribute(types.AttributeValueSuccess, types.Success),
			),
		)
		logger := k.Logger(ctx)
		logger.Debug(
			"reinitialize pool",
			"msg", msg,
			"pool", pool,
			"reserveCoins", reserveCoins,
			"lastReserveRatio", lastReserveRatio,
		)

		return nil
	}

	reserveCoins.Sort()

	lastReserveCoinA := reserveCoins[0]
	lastReserveCoinB := reserveCoins[1]

	depositCoinA := depositCoins[0]
	depositCoinB := depositCoins[1]

	poolCoinTotalSupply := k.GetPoolCoinTotalSupply(ctx, pool).ToLegacyDec()
	if err := types.CheckOverflowWithDec(poolCoinTotalSupply, depositCoinA.Amount.ToLegacyDec()); err != nil {
		return err
	}
	if err := types.CheckOverflowWithDec(poolCoinTotalSupply, depositCoinB.Amount.ToLegacyDec()); err != nil {
		return err
	}
	poolCoinMintAmt := math.LegacyMinDec(
		poolCoinTotalSupply.MulTruncate(depositCoinA.Amount.ToLegacyDec()).QuoTruncate(lastReserveCoinA.Amount.ToLegacyDec()),
		poolCoinTotalSupply.MulTruncate(depositCoinB.Amount.ToLegacyDec()).QuoTruncate(lastReserveCoinB.Amount.ToLegacyDec()),
	)
	mintRate := poolCoinMintAmt.TruncateDec().QuoTruncate(poolCoinTotalSupply)
	acceptedCoins := sdk.NewCoins(
		sdk.NewCoin(depositCoins[0].Denom, lastReserveCoinA.Amount.ToLegacyDec().Mul(mintRate).TruncateInt()),
		sdk.NewCoin(depositCoins[1].Denom, lastReserveCoinB.Amount.ToLegacyDec().Mul(mintRate).TruncateInt()),
	)
	refundedCoins := depositCoins.Sub(acceptedCoins...)
	refundedCoinA := sdk.NewCoin(depositCoinA.Denom, refundedCoins.AmountOf(depositCoinA.Denom))
	refundedCoinB := sdk.NewCoin(depositCoinB.Denom, refundedCoins.AmountOf(depositCoinB.Denom))

	mintPoolCoin := sdk.NewCoin(pool.PoolCoinDenom, poolCoinMintAmt.TruncateInt())
	mintPoolCoins := sdk.NewCoins(mintPoolCoin)

	if mintPoolCoins.IsZero() || acceptedCoins.IsZero() {
		return fmt.Errorf("pool coin truncated, no accepted coin, refund")
	}

	if err := k.bankKeeper.MintCoins(ctx, types.ModuleName, mintPoolCoins); err != nil {
		return err
	}

	if !refundedCoins.IsZero() {
		// refund truncated deposit coins
		inputs := banktypes.NewInput(batchEscrowAcc, refundedCoins)
		outputs := []banktypes.Output{banktypes.NewOutput(depositor, refundedCoins)}
		if err := k.bankKeeper.InputOutputCoins(ctx, inputs, outputs); err != nil {
			return err
		}
	}

	// send accepted deposit coins
	input2 := banktypes.NewInput(batchEscrowAcc, acceptedCoins)
	outputs2 := []banktypes.Output{banktypes.NewOutput(reserveAcc, acceptedCoins)}
	if err := k.bankKeeper.InputOutputCoins(ctx, input2, outputs2); err != nil {
		return err
	}

	// send minted pool coins
	input3 := banktypes.NewInput(batchEscrowAcc, mintPoolCoins)
	outputs3 := []banktypes.Output{banktypes.NewOutput(depositor, mintPoolCoins)}
	if err := k.bankKeeper.InputOutputCoins(ctx, input3, outputs3); err != nil {
		return err
	}

	msg.Succeeded = true
	msg.ToBeDeleted = true
	k.SetPoolBatchDepositMsgState(ctx, msg.Msg.PoolId, msg)

	if BatchLogicInvariantCheckFlag {
		afterReserveCoins := k.GetReserveCoins(ctx, pool)
		afterReserveCoinA := afterReserveCoins[0].Amount
		afterReserveCoinB := afterReserveCoins[1].Amount

		MintingPoolCoinsInvariant(poolCoinTotalSupply.TruncateInt(), mintPoolCoin.Amount, depositCoinA.Amount, depositCoinB.Amount,
			lastReserveCoinA.Amount, lastReserveCoinB.Amount, refundedCoinA.Amount, refundedCoinB.Amount)
		DepositInvariant(lastReserveCoinA.Amount, lastReserveCoinB.Amount, depositCoinA.Amount, depositCoinB.Amount,
			afterReserveCoinA, afterReserveCoinB, refundedCoinA.Amount, refundedCoinB.Amount)
	}

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeDepositToPool,
			sdk.NewAttribute(types.AttributeValuePoolId, strconv.FormatUint(pool.Id, 10)),
			sdk.NewAttribute(types.AttributeValueBatchIndex, strconv.FormatUint(batch.Index, 10)),
			sdk.NewAttribute(types.AttributeValueMsgIndex, strconv.FormatUint(msg.MsgIndex, 10)),
			sdk.NewAttribute(types.AttributeValueDepositor, depositor.String()),
			sdk.NewAttribute(types.AttributeValueAcceptedCoins, acceptedCoins.String()),
			sdk.NewAttribute(types.AttributeValueRefundedCoins, refundedCoins.String()),
			sdk.NewAttribute(types.AttributeValuePoolCoinDenom, mintPoolCoin.Denom),
			sdk.NewAttribute(types.AttributeValuePoolCoinAmount, mintPoolCoin.Amount.String()),
			sdk.NewAttribute(types.AttributeValueSuccess, types.Success),
		),
	)

	reserveCoins = k.GetReserveCoins(ctx, pool)
	lastReserveRatio := math.LegacyNewDecFromInt(reserveCoins[0].Amount).Quo(math.LegacyNewDecFromInt(reserveCoins[1].Amount))

	logger := k.Logger(ctx)
	logger.Debug(
		"deposit coins to the pool",
		"msg", msg,
		"pool", pool,
		"reserveCoins", reserveCoins,
		"lastReserveRatio", lastReserveRatio,
	)

	return nil
}

// ExecuteWithdrawal withdraws pool coin from the liquidity pool
func (k Keeper) ExecuteWithdrawal(ctx sdk.Context, msg types.WithdrawMsgState, batch types.PoolBatch) error {
	if msg.Executed || msg.ToBeDeleted || msg.Succeeded {
		return fmt.Errorf("cannot process already executed batch msg")
	}
	msg.Executed = true
	k.SetPoolBatchWithdrawMsgState(ctx, msg.Msg.PoolId, msg)

	if err := k.ValidateMsgWithdrawWithinBatch(ctx, *msg.Msg); err != nil {
		return err
	}
	poolCoins := sdk.NewCoins(msg.Msg.PoolCoin)

	pool, found := k.GetPool(ctx, msg.Msg.PoolId)
	if !found {
		return types.ErrPoolNotExists
	}

	poolCoinTotalSupply := k.GetPoolCoinTotalSupply(ctx, pool)
	reserveCoins := k.GetReserveCoins(ctx, pool)
	reserveCoins.Sort()

	var input banktypes.Input
	var outputs []banktypes.Output

	reserveAcc := pool.GetReserveAccount()
	withdrawer := msg.Msg.GetWithdrawer()

	params := k.GetParams(ctx)
	withdrawProportion := math.LegacyOneDec().Sub(params.WithdrawFeeRate)
	withdrawCoins := sdk.NewCoins()
	withdrawFeeCoins := sdk.NewCoins()

	// Case for withdrawing all reserve coins
	if msg.Msg.PoolCoin.Amount.Equal(poolCoinTotalSupply) {
		withdrawCoins = reserveCoins
	} else {
		// Calculate withdraw amount of respective reserve coin considering fees and pool coin's totally supply
		for _, reserveCoin := range reserveCoins {
			if err := types.CheckOverflow(reserveCoin.Amount, msg.Msg.PoolCoin.Amount); err != nil {
				return err
			}
			if err := types.CheckOverflow(reserveCoin.Amount.Mul(msg.Msg.PoolCoin.Amount).ToLegacyDec().TruncateInt(), poolCoinTotalSupply); err != nil {
				return err
			}
			// WithdrawAmount = ReserveAmount * PoolCoinAmount * WithdrawFeeProportion / TotalSupply
			withdrawAmtWithFee := reserveCoin.Amount.Mul(msg.Msg.PoolCoin.Amount).ToLegacyDec().TruncateInt().Quo(poolCoinTotalSupply)
			withdrawAmt := reserveCoin.Amount.Mul(msg.Msg.PoolCoin.Amount).ToLegacyDec().MulTruncate(withdrawProportion).TruncateInt().Quo(poolCoinTotalSupply)
			withdrawCoins = append(withdrawCoins, sdk.NewCoin(reserveCoin.Denom, withdrawAmt))
			withdrawFeeCoins = append(withdrawFeeCoins, sdk.NewCoin(reserveCoin.Denom, withdrawAmtWithFee.Sub(withdrawAmt)))
		}
	}

	if withdrawCoins.IsValid() {
		input = banktypes.NewInput(reserveAcc, withdrawCoins)
		outputs = append(outputs, banktypes.NewOutput(withdrawer, withdrawCoins))
	} else {
		return types.ErrBadPoolCoinAmount
	}

	// send withdrawing coins to the withdrawer
	if err := k.bankKeeper.InputOutputCoins(ctx, input, outputs); err != nil {
		return err
	}

	// burn the escrowed pool coins
	if err := k.bankKeeper.BurnCoins(ctx, types.ModuleName, poolCoins); err != nil {
		return err
	}

	msg.Succeeded = true
	msg.ToBeDeleted = true
	k.SetPoolBatchWithdrawMsgState(ctx, msg.Msg.PoolId, msg)

	if BatchLogicInvariantCheckFlag {
		afterPoolCoinTotalSupply := k.GetPoolCoinTotalSupply(ctx, pool)
		afterReserveCoins := k.GetReserveCoins(ctx, pool)
		afterReserveCoinA := math.ZeroInt()
		afterReserveCoinB := math.ZeroInt()
		if !afterReserveCoins.IsZero() {
			afterReserveCoinA = afterReserveCoins[0].Amount
			afterReserveCoinB = afterReserveCoins[1].Amount
		}
		burnedPoolCoin := poolCoins[0].Amount
		withdrawCoinA := withdrawCoins[0].Amount
		withdrawCoinB := withdrawCoins[1].Amount
		reserveCoinA := reserveCoins[0].Amount
		reserveCoinB := reserveCoins[1].Amount
		lastPoolCoinTotalSupply := poolCoinTotalSupply
		afterPoolTotalSupply := afterPoolCoinTotalSupply

		BurningPoolCoinsInvariant(burnedPoolCoin, withdrawCoinA, withdrawCoinB, reserveCoinA, reserveCoinB, lastPoolCoinTotalSupply, withdrawFeeCoins)
		WithdrawReserveCoinsInvariant(withdrawCoinA, withdrawCoinB, reserveCoinA, reserveCoinB,
			afterReserveCoinA, afterReserveCoinB, afterPoolTotalSupply, lastPoolCoinTotalSupply, burnedPoolCoin)
		WithdrawAmountInvariant(withdrawCoinA, withdrawCoinB, reserveCoinA, reserveCoinB, burnedPoolCoin, lastPoolCoinTotalSupply, params.WithdrawFeeRate)
		ImmutablePoolPriceAfterWithdrawInvariant(reserveCoinA, reserveCoinB, withdrawCoinA, withdrawCoinB, afterReserveCoinA, afterReserveCoinB)
	}

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeWithdrawFromPool,
			sdk.NewAttribute(types.AttributeValuePoolId, strconv.FormatUint(pool.Id, 10)),
			sdk.NewAttribute(types.AttributeValueBatchIndex, strconv.FormatUint(batch.Index, 10)),
			sdk.NewAttribute(types.AttributeValueMsgIndex, strconv.FormatUint(msg.MsgIndex, 10)),
			sdk.NewAttribute(types.AttributeValueWithdrawer, withdrawer.String()),
			sdk.NewAttribute(types.AttributeValuePoolCoinDenom, msg.Msg.PoolCoin.Denom),
			sdk.NewAttribute(types.AttributeValuePoolCoinAmount, msg.Msg.PoolCoin.Amount.String()),
			sdk.NewAttribute(types.AttributeValueWithdrawCoins, withdrawCoins.String()),
			sdk.NewAttribute(types.AttributeValueWithdrawFeeCoins, withdrawFeeCoins.String()),
			sdk.NewAttribute(types.AttributeValueSuccess, types.Success),
		),
	)

	reserveCoins = k.GetReserveCoins(ctx, pool)

	var lastReserveRatio math.LegacyDec
	if reserveCoins.IsZero() {
		lastReserveRatio = math.LegacyZeroDec()
	} else {
		lastReserveRatio = math.LegacyNewDecFromInt(reserveCoins[0].Amount).Quo(math.LegacyNewDecFromInt(reserveCoins[1].Amount))
	}

	logger := k.Logger(ctx)
	logger.Debug(
		"withdraw pool coin from the pool",
		"msg", msg,
		"pool", pool,
		"reserveCoins", reserveCoins,
		"lastReserveRatio", lastReserveRatio,
	)

	return nil
}

// GetPoolCoinTotalSupply returns total supply of pool coin of the pool in form of sdk.Int
func (k Keeper) GetPoolCoinTotalSupply(ctx sdk.Context, pool types.Pool) math.Int {
	return k.bankKeeper.GetSupply(ctx, pool.PoolCoinDenom).Amount
}

// IsDepletedPool returns true if the pool is depleted.
func (k Keeper) IsDepletedPool(ctx sdk.Context, pool types.Pool) bool {
	reserveCoins := k.GetReserveCoins(ctx, pool)
	return !k.GetPoolCoinTotalSupply(ctx, pool).IsPositive() ||
		reserveCoins.AmountOf(pool.ReserveCoinDenoms[0]).IsZero() ||
		reserveCoins.AmountOf(pool.ReserveCoinDenoms[1]).IsZero()
}

// GetPoolCoinTotal returns total supply of pool coin of the pool in form of sdk.Coin
func (k Keeper) GetPoolCoinTotal(ctx sdk.Context, pool types.Pool) sdk.Coin {
	return sdk.NewCoin(pool.PoolCoinDenom, k.GetPoolCoinTotalSupply(ctx, pool))
}

// GetReserveCoins returns reserve coins from the liquidity pool
func (k Keeper) GetReserveCoins(ctx sdk.Context, pool types.Pool) (reserveCoins sdk.Coins) {
	reserveAcc := pool.GetReserveAccount()
	reserveCoins = sdk.NewCoins()
	for _, denom := range pool.ReserveCoinDenoms {
		reserveCoins = append(reserveCoins, k.bankKeeper.GetBalance(ctx, reserveAcc, denom))
	}
	return
}

func (k Keeper) CalculateOutputAmount(ctx sdk.Context, pool types.Pool, inputAmount sdk.DecCoin) (math.LegacyDec, error) {

	// get reserve coins from the liquidity pool and calculate the current pool price (p = x / y)
	reserveCoins := k.GetReserveCoins(ctx, pool)
	X := reserveCoins[0].Amount.ToLegacyDec()
	Y := reserveCoins[1].Amount.ToLegacyDec()

	if (inputAmount.Denom != reserveCoins[0].Denom) && (inputAmount.Denom != reserveCoins[1].Denom) {
		return math.LegacyDec{}, types.ErrInvalidDenom
	}

	inputReserve := X
	outputReserve := Y
	if inputAmount.Denom == reserveCoins[1].Denom {
		inputReserve = Y
		outputReserve = X
	}
	outputAmount := outputReserve.Mul(inputAmount.Amount)
	outputAmount = outputAmount.Quo(inputReserve.Add(inputAmount.Amount))

	return outputAmount, nil
}

// GetPoolMetaData returns metadata of the pool
func (k Keeper) GetPoolMetaData(ctx sdk.Context, pool types.Pool) types.PoolMetadata {
	return types.PoolMetadata{
		PoolId:              pool.Id,
		PoolCoinTotalSupply: k.GetPoolCoinTotal(ctx, pool),
		ReserveCoins:        k.GetReserveCoins(ctx, pool),
	}
}

// GetPoolRecord returns the liquidity pool record with the given pool information
func (k Keeper) GetPoolRecord(ctx sdk.Context, pool types.Pool) (types.PoolRecord, bool) {
	batch, found := k.GetPoolBatch(ctx, pool.Id)
	if !found {
		return types.PoolRecord{}, false
	}
	return types.PoolRecord{
		Pool:              pool,
		PoolMetadata:      k.GetPoolMetaData(ctx, pool),
		PoolBatch:         batch,
		DepositMsgStates:  k.GetAllPoolBatchDepositMsgs(ctx, batch),
		WithdrawMsgStates: k.GetAllPoolBatchWithdrawMsgStates(ctx, batch),
		SwapMsgStates:     k.GetAllPoolBatchSwapMsgStates(ctx, batch),
	}, true
}

// SetPoolRecord stores liquidity pool states
func (k Keeper) SetPoolRecord(ctx sdk.Context, record types.PoolRecord) types.PoolRecord {
	k.SetPoolAtomic(ctx, record.Pool)
	if record.PoolBatch.BeginHeight > ctx.BlockHeight() {
		record.PoolBatch.BeginHeight = 0
	}
	k.SetPoolBatch(ctx, record.PoolBatch)
	k.SetPoolBatchDepositMsgStates(ctx, record.Pool.Id, record.DepositMsgStates)
	k.SetPoolBatchWithdrawMsgStates(ctx, record.Pool.Id, record.WithdrawMsgStates)
	k.SetPoolBatchSwapMsgStates(ctx, record.Pool.Id, record.SwapMsgStates)
	return record
}

// RefundDeposit refunds deposit amounts to the depositor
func (k Keeper) RefundDeposit(ctx sdk.Context, batchMsg types.DepositMsgState, batch types.PoolBatch) error {
	batchMsg, _ = k.GetPoolBatchDepositMsgState(ctx, batchMsg.Msg.PoolId, batchMsg.MsgIndex)
	if !batchMsg.Executed || batchMsg.Succeeded {
		return fmt.Errorf("cannot refund not executed or already succeeded msg")
	}
	pool, _ := k.GetPool(ctx, batchMsg.Msg.PoolId)
	if err := k.ReleaseEscrow(ctx, batchMsg.Msg.GetDepositor(), batchMsg.Msg.DepositCoins); err != nil {
		return err
	}
	// not delete now, set ToBeDeleted true for delete on next block beginblock
	batchMsg.ToBeDeleted = true
	k.SetPoolBatchDepositMsgState(ctx, batchMsg.Msg.PoolId, batchMsg)
	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeDepositToPool,
			sdk.NewAttribute(types.AttributeValuePoolId, strconv.FormatUint(pool.Id, 10)),
			sdk.NewAttribute(types.AttributeValueBatchIndex, strconv.FormatUint(batch.Index, 10)),
			sdk.NewAttribute(types.AttributeValueMsgIndex, strconv.FormatUint(batchMsg.MsgIndex, 10)),
			sdk.NewAttribute(types.AttributeValueDepositor, batchMsg.Msg.GetDepositor().String()),
			sdk.NewAttribute(types.AttributeValueAcceptedCoins, sdk.NewCoins().String()),
			sdk.NewAttribute(types.AttributeValueRefundedCoins, batchMsg.Msg.DepositCoins.String()),
			sdk.NewAttribute(types.AttributeValueSuccess, types.Failure),
		))
	return nil
}

// RefundWithdrawal refunds pool coin of the liquidity pool to the withdrawer
func (k Keeper) RefundWithdrawal(ctx sdk.Context, batchMsg types.WithdrawMsgState, batch types.PoolBatch) error {
	batchMsg, _ = k.GetPoolBatchWithdrawMsgState(ctx, batchMsg.Msg.PoolId, batchMsg.MsgIndex)
	if !batchMsg.Executed || batchMsg.Succeeded {
		return fmt.Errorf("cannot refund not executed or already succeeded msg")
	}
	pool, _ := k.GetPool(ctx, batchMsg.Msg.PoolId)
	if err := k.ReleaseEscrow(ctx, batchMsg.Msg.GetWithdrawer(), sdk.NewCoins(batchMsg.Msg.PoolCoin)); err != nil {
		return err
	}
	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeWithdrawFromPool,
			sdk.NewAttribute(types.AttributeValuePoolId, strconv.FormatUint(pool.Id, 10)),
			sdk.NewAttribute(types.AttributeValueBatchIndex, strconv.FormatUint(batch.Index, 10)),
			sdk.NewAttribute(types.AttributeValueMsgIndex, strconv.FormatUint(batchMsg.MsgIndex, 10)),
			sdk.NewAttribute(types.AttributeValueWithdrawer, batchMsg.Msg.GetWithdrawer().String()),
			sdk.NewAttribute(types.AttributeValuePoolCoinDenom, batchMsg.Msg.PoolCoin.Denom),
			sdk.NewAttribute(types.AttributeValuePoolCoinAmount, batchMsg.Msg.PoolCoin.Amount.String()),
			sdk.NewAttribute(types.AttributeValueSuccess, types.Failure),
		))

	// not delete now, set ToBeDeleted true for delete on next block beginblock
	batchMsg.ToBeDeleted = true
	k.SetPoolBatchWithdrawMsgState(ctx, batchMsg.Msg.PoolId, batchMsg)
	return nil
}

func (k Keeper) SendAmountToBuilders(params types.Params, from sdk.AccAddress, totalCommission sdk.Coin, sendCoin func(from, to sdk.AccAddress, coin sdk.Coin) error) (sdk.Coin, error) {
	remainingAmount := totalCommission
	if len(params.BuildersAddresses) > 0 {
		totalDecCommission := sdk.NewDecCoinFromCoin(totalCommission)
		for _, builderAddr := range params.BuildersAddresses {
			builderCommPerAcc := totalDecCommission.Amount.Mul(builderAddr.Weight).TruncateInt()
			builderAcc := sdk.MustAccAddressFromBech32(builderAddr.Address)
			remainingAmount = remainingAmount.SubAmount(builderCommPerAcc)
			err := sendCoin(from, builderAcc, sdk.NewCoin(totalCommission.Denom, builderCommPerAcc))
			if err != nil {
				return sdk.Coin{}, err
			}
		}
	}
	return remainingAmount, nil
}

// TransactAndRefundSwapLiquidityPool transacts, refunds, expires, sends coins with escrow, update state by TransactAndRefundSwapLiquidityPool
func (k Keeper) TransactAndRefundSwapLiquidityPool(ctx sdk.Context, swapMsgStates []*types.SwapMsgState,
	matchResultMap map[uint64]types.MatchResult, pool types.Pool, batchResult types.BatchResult) error {
	batchEscrowAcc := k.accountKeeper.GetModuleAddress(types.ModuleName)
	poolReserveAcc := pool.GetReserveAccount()
	batch, found := k.GetPoolBatch(ctx, pool.Id)
	params := k.GetParams(ctx)
	if !found {
		return types.ErrPoolBatchNotExists
	}
	sendCoin := func(from, to sdk.AccAddress, coin sdk.Coin) error {
		coins := sdk.NewCoins(coin)

		if !coins.Empty() && coins.IsValid() {
			var input banktypes.Input
			var outputs []banktypes.Output
			input = banktypes.NewInput(from, coins)
			outputs = append(outputs, banktypes.NewOutput(to, coins))
			if err := k.bankKeeper.InputOutputCoins(ctx, input, outputs); err != nil {
				return err
			}
		}
		return nil
	}
	var builders bool = params.BuildersAddresses != nil && len(params.BuildersAddresses) > 0
	for _, sms := range swapMsgStates {
		if pool.Id != sms.Msg.PoolId {
			return fmt.Errorf("broken msg pool consistency")
		}
		if !sms.Executed && sms.Succeeded {
			return fmt.Errorf("can't refund not executed with succeed msg")
		}
		if sms.RemainingOfferCoin.IsNegative() {
			return fmt.Errorf("negative RemainingOfferCoin")
		} else if sms.RemainingOfferCoin.IsPositive() &&
			((!sms.ToBeDeleted && sms.OrderExpiryHeight <= ctx.BlockHeight()) ||
				(sms.ToBeDeleted && sms.OrderExpiryHeight != ctx.BlockHeight())) {
			return fmt.Errorf("consistency of OrderExpiryHeight and ToBeDeleted flag is broken")
		}

		if match, ok := matchResultMap[sms.MsgIndex]; ok {
			transactedAmt := match.TransactedCoinAmt.TruncateInt()
			receiveAmt := match.ExchangedDemandCoinAmt.Sub(match.ExchangedCoinFeeAmt).TruncateInt()
			offerCoinFeeAmt := match.OfferCoinFeeAmt.TruncateInt()

			if builders {
				builderCommOfferAmount := offerCoinFeeAmt.ToLegacyDec().Mul(params.BuildersCommission).TruncateInt()
				builderCommDemandAmount := match.ExchangedCoinFeeAmt.Mul(params.BuildersCommission).TruncateInt()
				offerCoinFeeAmt = match.OfferCoinFeeAmt.Sub(builderCommOfferAmount.ToLegacyDec()).TruncateInt()
				remainingAmount, err := k.SendAmountToBuilders(params, batchEscrowAcc, sdk.NewCoin(sms.Msg.OfferCoin.Denom, builderCommOfferAmount), sendCoin)
				if err != nil {
					return err
				}
				offerCoinFeeAmt = offerCoinFeeAmt.Add(remainingAmount.Amount)
				_, error := k.SendAmountToBuilders(params, poolReserveAcc, sdk.NewCoin(sms.Msg.DemandCoinDenom, builderCommDemandAmount), sendCoin)
				if error != nil {
					return error
				}
			}

			err := sendCoin(batchEscrowAcc, poolReserveAcc, sdk.NewCoin(sms.Msg.OfferCoin.Denom, transactedAmt))
			err = sendCoin(poolReserveAcc, sms.Msg.GetSwapRequester(), sdk.NewCoin(sms.Msg.DemandCoinDenom, receiveAmt))
			err = sendCoin(batchEscrowAcc, poolReserveAcc, sdk.NewCoin(sms.Msg.OfferCoin.Denom, offerCoinFeeAmt))
			if err != nil {
				return err
			}

			if sms.RemainingOfferCoin.Add(sms.ReservedOfferCoinFee).IsPositive() && sms.OrderExpiryHeight == ctx.BlockHeight() {
				err := sendCoin(batchEscrowAcc, sms.Msg.GetSwapRequester(), sms.RemainingOfferCoin.Add(sms.ReservedOfferCoinFee))
				if err != nil {
					return err
				}
			}

			sms.Succeeded = true
			if sms.RemainingOfferCoin.IsZero() {
				sms.ToBeDeleted = true
			}

			ctx.EventManager().EmitEvent(
				sdk.NewEvent(
					types.EventTypeSwapTransacted,
					sdk.NewAttribute(types.AttributeValuePoolId, strconv.FormatUint(pool.Id, 10)),
					sdk.NewAttribute(types.AttributeValueBatchIndex, strconv.FormatUint(batch.Index, 10)),
					sdk.NewAttribute(types.AttributeValueMsgIndex, strconv.FormatUint(sms.MsgIndex, 10)),
					sdk.NewAttribute(types.AttributeValueSwapRequester, sms.Msg.GetSwapRequester().String()),
					sdk.NewAttribute(types.AttributeValueSwapTypeId, strconv.FormatUint(uint64(sms.Msg.SwapTypeId), 10)),
					sdk.NewAttribute(types.AttributeValueOfferCoinDenom, sms.Msg.OfferCoin.Denom),
					sdk.NewAttribute(types.AttributeValueOfferCoinAmount, sms.Msg.OfferCoin.Amount.String()),
					sdk.NewAttribute(types.AttributeValueDemandCoinDenom, sms.Msg.DemandCoinDenom),
					sdk.NewAttribute(types.AttributeValueOrderPrice, sms.Msg.OrderPrice.String()),
					sdk.NewAttribute(types.AttributeValueSwapPrice, batchResult.SwapPrice.String()),
					sdk.NewAttribute(types.AttributeValueTransactedCoinAmount, transactedAmt.String()),
					sdk.NewAttribute(types.AttributeValueRemainingOfferCoinAmount, sms.RemainingOfferCoin.Amount.String()),
					sdk.NewAttribute(types.AttributeValueExchangedOfferCoinAmount, sms.ExchangedOfferCoin.Amount.String()),
					sdk.NewAttribute(types.AttributeValueExchangedDemandCoinAmount, receiveAmt.String()),
					sdk.NewAttribute(types.AttributeValueOfferCoinFeeAmount, offerCoinFeeAmt.String()),
					sdk.NewAttribute(types.AttributeValueExchangedCoinFeeAmount, match.ExchangedCoinFeeAmt.String()),
					sdk.NewAttribute(types.AttributeValueReservedOfferCoinFeeAmount, sms.ReservedOfferCoinFee.Amount.String()),
					sdk.NewAttribute(types.AttributeValueOrderExpiryHeight, strconv.FormatInt(sms.OrderExpiryHeight, 10)),
					sdk.NewAttribute(types.AttributeValueSuccess, types.Success),
				))
		} else {
			// Not matched, remaining
			err := sendCoin(batchEscrowAcc, sms.Msg.GetSwapRequester(), sms.RemainingOfferCoin.Add(sms.ReservedOfferCoinFee))
			if err != nil {
				return err
			}
			sms.Succeeded = false
			sms.ToBeDeleted = true

			ctx.EventManager().EmitEvent(
				sdk.NewEvent(
					types.EventTypeSwapTransacted,
					sdk.NewAttribute(types.AttributeValuePoolId, strconv.FormatUint(pool.Id, 10)),
					sdk.NewAttribute(types.AttributeValueBatchIndex, strconv.FormatUint(batch.Index, 10)),
					sdk.NewAttribute(types.AttributeValueMsgIndex, strconv.FormatUint(sms.MsgIndex, 10)),
					sdk.NewAttribute(types.AttributeValueSwapRequester, sms.Msg.GetSwapRequester().String()),
					sdk.NewAttribute(types.AttributeValueSwapTypeId, strconv.FormatUint(uint64(sms.Msg.SwapTypeId), 10)),
					sdk.NewAttribute(types.AttributeValueOfferCoinDenom, sms.Msg.OfferCoin.Denom),
					sdk.NewAttribute(types.AttributeValueOfferCoinAmount, sms.Msg.OfferCoin.Amount.String()),
					sdk.NewAttribute(types.AttributeValueDemandCoinDenom, sms.Msg.DemandCoinDenom),
					sdk.NewAttribute(types.AttributeValueOrderPrice, sms.Msg.OrderPrice.String()),
					sdk.NewAttribute(types.AttributeValueSwapPrice, batchResult.SwapPrice.String()),
					sdk.NewAttribute(types.AttributeValueRemainingOfferCoinAmount, sms.RemainingOfferCoin.Amount.String()),
					sdk.NewAttribute(types.AttributeValueExchangedOfferCoinAmount, sms.ExchangedOfferCoin.Amount.String()),
					sdk.NewAttribute(types.AttributeValueReservedOfferCoinFeeAmount, sms.ReservedOfferCoinFee.Amount.String()),
					sdk.NewAttribute(types.AttributeValueOrderExpiryHeight, strconv.FormatInt(sms.OrderExpiryHeight, 10)),
					sdk.NewAttribute(types.AttributeValueSuccess, types.Failure),
					sdk.NewAttribute(types.AttributeValueNoMatch, types.Failure),
				))

		}
	}
	k.SetPoolBatchSwapMsgStatesByPointer(ctx, pool.Id, swapMsgStates)
	return nil
}

func (k Keeper) RefundSwaps(ctx sdk.Context, pool types.Pool, swapMsgStates []*types.SwapMsgState) error {
	sendCoin := func(from, to sdk.AccAddress, coin sdk.Coin) error {
		coins := sdk.NewCoins(coin)

		if !coins.Empty() && coins.IsValid() {
			var input banktypes.Input
			var outputs []banktypes.Output
			input = banktypes.NewInput(from, coins)
			outputs = append(outputs, banktypes.NewOutput(to, coins))
			if err := k.bankKeeper.InputOutputCoins(ctx, input, outputs); err != nil {
				return err
			}
		}
		return nil
	}
	for _, sms := range swapMsgStates {
		if sms.OrderExpiryHeight == ctx.BlockHeight() {
			err := sendCoin(k.accountKeeper.GetModuleAddress(types.ModuleName), sms.Msg.GetSwapRequester(), sms.RemainingOfferCoin.Add(sms.ReservedOfferCoinFee))
			if err != nil {
				return err
			}
			sms.Succeeded = false
			sms.ToBeDeleted = true
		}
	}

	k.SetPoolBatchSwapMsgStatesByPointer(ctx, pool.Id, swapMsgStates)
	return nil
}

// ValidateMsgDepositWithinBatch validates MsgDepositWithinBatch
func (k Keeper) ValidateMsgDepositWithinBatch(ctx sdk.Context, msg types.MsgDepositWithinBatch) error {
	pool, found := k.GetPool(ctx, msg.PoolId)
	if !found {
		return types.ErrPoolNotExists
	}

	if msg.DepositCoins.Len() != len(pool.ReserveCoinDenoms) {
		return types.ErrNumOfReserveCoin
	}

	params := k.GetParams(ctx)
	reserveCoins := k.GetReserveCoins(ctx, pool)
	if err := types.ValidateReserveCoinLimit(params.MaxReserveCoinAmount, reserveCoins.Add(msg.DepositCoins...)); err != nil {
		return err
	}

	denomA, denomB := types.AlphabeticalDenomPair(msg.DepositCoins[0].Denom, msg.DepositCoins[1].Denom)
	if denomA != pool.ReserveCoinDenoms[0] || denomB != pool.ReserveCoinDenoms[1] {
		return types.ErrNotMatchedReserveCoin
	}
	return nil
}

// ValidateMsgWithdrawWithinBatch validates MsgWithdrawWithinBatch
func (k Keeper) ValidateMsgWithdrawWithinBatch(ctx sdk.Context, msg types.MsgWithdrawWithinBatch) error {
	pool, found := k.GetPool(ctx, msg.PoolId)
	if !found {
		return types.ErrPoolNotExists
	}

	if msg.PoolCoin.Denom != pool.PoolCoinDenom {
		return types.ErrBadPoolCoinDenom
	}

	poolCoinTotalSupply := k.GetPoolCoinTotalSupply(ctx, pool)
	if k.IsDepletedPool(ctx, pool) {
		return types.ErrDepletedPool
	}

	if msg.PoolCoin.Amount.GT(poolCoinTotalSupply) {
		return types.ErrBadPoolCoinAmount
	}
	return nil
}

func (k Keeper) ValidateDirectSwap(ctx sdk.Context, offerCoin sdk.Coin, demandDenom string, orderPrice math.LegacyDec, pool types.Pool) error {
	denomA, denomB := types.AlphabeticalDenomPair(offerCoin.Denom, demandDenom)
	if denomA != pool.ReserveCoinDenoms[0] || denomB != pool.ReserveCoinDenoms[1] {
		return types.ErrNotMatchedReserveCoin
	}

	params := k.GetParams(ctx)

	// can not exceed max order ratio  of reserve coins that can be ordered at a order
	reserveCoinAmt := k.GetReserveCoins(ctx, pool).AmountOf(offerCoin.Denom)

	// Decimal Error, Multiply the Int coin amount by the Decimal Rate and erase the decimal point to order a lower value
	maximumOrderableAmt := reserveCoinAmt.ToLegacyDec().MulTruncate(params.MaxOrderAmountRatio).TruncateInt()
	if offerCoin.Amount.GT(maximumOrderableAmt) {
		return types.ErrExceededMaxOrderable
	}

	if err := types.CheckOverflowWithDec(offerCoin.Amount.ToLegacyDec(), orderPrice); err != nil {
		return err
	}

	return nil
}

// ValidateMsgSwapWithinBatch validates MsgSwapWithinBatch.
func (k Keeper) ValidateMsgSwapWithinBatch(ctx sdk.Context, msg types.MsgSwapWithinBatch, pool types.Pool) error {
	denomA, denomB := types.AlphabeticalDenomPair(msg.OfferCoin.Denom, msg.DemandCoinDenom)
	if denomA != pool.ReserveCoinDenoms[0] || denomB != pool.ReserveCoinDenoms[1] {
		return types.ErrNotMatchedReserveCoin
	}

	params := k.GetParams(ctx)

	// can not exceed max order ratio  of reserve coins that can be ordered at a order
	reserveCoinAmt := k.GetReserveCoins(ctx, pool).AmountOf(msg.OfferCoin.Denom)

	// Decimal Error, Multiply the Int coin amount by the Decimal Rate and erase the decimal point to order a lower value
	maximumOrderableAmt := reserveCoinAmt.ToLegacyDec().MulTruncate(params.MaxOrderAmountRatio).TruncateInt()
	if msg.OfferCoin.Amount.GT(maximumOrderableAmt) {
		return types.ErrExceededMaxOrderable
	}

	if msg.OfferCoinFee.Denom != msg.OfferCoin.Denom {
		return types.ErrBadOfferCoinFee
	}

	if err := types.CheckOverflowWithDec(msg.OfferCoin.Amount.ToLegacyDec(), msg.OrderPrice); err != nil {
		return err
	}

	if !msg.OfferCoinFee.Equal(types.GetOfferCoinFee(msg.OfferCoin, params.SwapFeeRate)) {
		return types.ErrBadOfferCoinFee
	}

	return nil
}

// ValidatePool validates logic for liquidity pool after set or before export
func (k Keeper) ValidatePool(ctx sdk.Context, pool *types.Pool) error {
	params := k.GetParams(ctx)
	var poolType types.PoolType

	// check poolType exist, get poolType from param
	if len(params.PoolTypes) >= int(pool.TypeId) {
		poolType = params.PoolTypes[pool.TypeId-1]
		if poolType.Id != pool.TypeId {
			return types.ErrPoolTypeNotExists
		}
	} else {
		return types.ErrPoolTypeNotExists
	}

	if poolType.MaxReserveCoinNum > types.MaxReserveCoinNum || types.MinReserveCoinNum > poolType.MinReserveCoinNum {
		return types.ErrNumOfReserveCoin
	}

	reserveCoins := k.GetReserveCoins(ctx, *pool)
	if uint32(reserveCoins.Len()) > poolType.MaxReserveCoinNum || poolType.MinReserveCoinNum > uint32(reserveCoins.Len()) {
		return types.ErrNumOfReserveCoin
	}

	if len(pool.ReserveCoinDenoms) != reserveCoins.Len() {
		return types.ErrNumOfReserveCoin
	}
	for i, denom := range pool.ReserveCoinDenoms {
		if denom != reserveCoins[i].Denom {
			return types.ErrInvalidDenom
		}
	}

	denomA, denomB := types.AlphabeticalDenomPair(pool.ReserveCoinDenoms[0], pool.ReserveCoinDenoms[1])
	if denomA != pool.ReserveCoinDenoms[0] || denomB != pool.ReserveCoinDenoms[1] {
		return types.ErrBadOrderingReserveCoin
	}

	poolName := types.PoolName(pool.ReserveCoinDenoms, pool.TypeId)
	poolCoin := k.GetPoolCoinTotal(ctx, *pool)
	if poolCoin.Denom != types.GetPoolCoinDenom(poolName) {
		return types.ErrBadPoolCoinDenom
	}

	_, found := k.GetPoolBatch(ctx, pool.Id)
	if !found {
		return types.ErrPoolBatchNotExists
	}

	return nil
}

// ValidatePoolMetadata validates logic for liquidity pool metadata
func (k Keeper) ValidatePoolMetadata(ctx sdk.Context, pool *types.Pool, metaData *types.PoolMetadata) error {
	if err := metaData.ReserveCoins.Validate(); err != nil {
		return err
	}
	if !metaData.ReserveCoins.Equal(k.GetReserveCoins(ctx, *pool)) {
		return types.ErrNumOfReserveCoin
	}
	if !metaData.PoolCoinTotalSupply.IsEqual(sdk.NewCoin(pool.PoolCoinDenom, k.GetPoolCoinTotalSupply(ctx, *pool))) {
		return types.ErrBadPoolCoinAmount
	}
	return nil
}

// ValidatePoolRecord validates liquidity pool record after init or after export
func (k Keeper) ValidatePoolRecord(ctx sdk.Context, record types.PoolRecord) error {
	if err := k.ValidatePool(ctx, &record.Pool); err != nil {
		return err
	}

	if err := k.ValidatePoolMetadata(ctx, &record.Pool, &record.PoolMetadata); err != nil {
		return err
	}

	if len(record.DepositMsgStates) != 0 && record.PoolBatch.DepositMsgIndex != record.DepositMsgStates[len(record.DepositMsgStates)-1].MsgIndex+1 {
		return types.ErrBadBatchMsgIndex
	}
	if len(record.WithdrawMsgStates) != 0 && record.PoolBatch.WithdrawMsgIndex != record.WithdrawMsgStates[len(record.WithdrawMsgStates)-1].MsgIndex+1 {
		return types.ErrBadBatchMsgIndex
	}
	if len(record.SwapMsgStates) != 0 && record.PoolBatch.SwapMsgIndex != record.SwapMsgStates[len(record.SwapMsgStates)-1].MsgIndex+1 {
		return types.ErrBadBatchMsgIndex
	}

	return nil
}

// IsPoolCoinDenom returns true if the denom is a valid pool coin denom.
func (k Keeper) IsPoolCoinDenom(ctx sdk.Context, denom string) bool {
	reserveAcc, err := types.GetReserveAcc(denom)
	if err != nil {
		return false
	}
	_, found := k.GetPoolByReserveAccIndex(ctx, reserveAcc)
	return found
}

func isWhitelistedAddress(sender string, params types.Params) bool {
	if len(params.GetPoolPermissionedCreatorAddresses()) > 0 {
		for _, addr := range params.GetPoolPermissionedCreatorAddresses() {
			if addr == sender {
				return true
			}
		}
	}
	return false
}
