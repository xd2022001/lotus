package sealing

import (
	"bytes"
	"context"
	"fmt"
	"math/bits"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/storage/pipeline/sealiface"
)

func fillersFromRem(in abi.UnpaddedPieceSize) ([]abi.UnpaddedPieceSize, error) {
	// Convert to in-sector bytes for easier math:
	//
	// Sector size to user bytes ratio is constant, e.g. for 1024B we have 1016B
	// of user-usable data.
	//
	// (1024/1016 = 128/127)
	//
	// Given that we can get sector size by simply adding 1/127 of the user
	// bytes
	//
	// (we convert to sector bytes as they are nice round binary numbers)

	toFill := uint64(in + (in / 127))

	// We need to fill the sector with pieces that are powers of 2. Conveniently
	// computers store numbers in binary, which means we can look at 1s to get
	// all the piece sizes we need to fill the sector. It also means that number
	// of pieces is the number of 1s in the number of remaining bytes to fill
	out := make([]abi.UnpaddedPieceSize, bits.OnesCount64(toFill))
	for i := range out {
		// Extract the next lowest non-zero bit
		next := bits.TrailingZeros64(toFill)
		psize := uint64(1) << next
		// e.g: if the number is 0b010100, psize will be 0b000100

		// set that bit to 0 by XORing it, so the next iteration looks at the
		// next bit
		toFill ^= psize

		// Add the piece size to the list of pieces we need to create
		out[i] = abi.PaddedPieceSize(psize).Unpadded()
	}
	return out, nil
}

func (m *Sealing) ListSectors() ([]SectorInfo, error) {
	var sectors []SectorInfo
	if err := m.sectors.List(&sectors); err != nil {
		return nil, err
	}
	return sectors, nil
}

func (m *Sealing) GetSectorInfo(sid abi.SectorNumber) (SectorInfo, error) {
	var out SectorInfo
	err := m.sectors.Get(uint64(sid)).Get(&out)
	return out, err
}

func collateralSendAmount(ctx context.Context, api interface {
	StateMinerAvailableBalance(context.Context, address.Address, types.TipSetKey) (big.Int, error)
}, maddr address.Address, cfg sealiface.Config, collateral abi.TokenAmount) (abi.TokenAmount, error) {
	if cfg.CollateralFromMinerBalance {
		if cfg.DisableCollateralFallback {
			return big.Zero(), nil
		}

		avail, err := api.StateMinerAvailableBalance(ctx, maddr, types.EmptyTSK)
		if err != nil {
			return big.Zero(), xerrors.Errorf("getting available miner balance: %w", err)
		}

		avail = big.Sub(avail, cfg.AvailableBalanceBuffer)
		if avail.LessThan(big.Zero()) {
			avail = big.Zero()
		}

		collateral = big.Sub(collateral, avail)
		if collateral.LessThan(big.Zero()) {
			collateral = big.Zero()
		}
	}

	return collateral, nil
}

func simulateMsgGas(ctx context.Context, sa interface {
	GasEstimateMessageGas(context.Context, *types.Message, *api.MessageSendSpec, types.TipSetKey) (*types.Message, error)
},
	from, to address.Address, method abi.MethodNum, value, maxFee abi.TokenAmount, params []byte) (*types.Message, error) {
	msg := types.Message{
		To:     to,
		From:   from,
		Value:  value,
		Method: method,
		Params: params,
	}

	var b bytes.Buffer
	err := msg.MarshalCBOR(&b)
	if err != nil {
		return nil, xerrors.Errorf("failed to unmarshal the signed message: %w", err)
	}

	gmsg, err := sa.GasEstimateMessageGas(ctx, &msg, nil, types.EmptyTSK)
	if err != nil {
		err = fmt.Errorf("message %x failed: %w", b.Bytes(), err)
	}
	return gmsg, err
}

func sendMsg(ctx context.Context, sa interface {
	MpoolPushMessage(context.Context, *types.Message, *api.MessageSendSpec) (*types.SignedMessage, error)
}, from, to address.Address, method abi.MethodNum, value, maxFee abi.TokenAmount, params []byte) (cid.Cid, error) {
	msg := types.Message{
		To:     to,
		From:   from,
		Value:  value,
		Method: method,
		Params: params,
	}

	smsg, err := sa.MpoolPushMessage(ctx, &msg, &api.MessageSendSpec{MaxFee: maxFee})
	if err != nil {
		return cid.Undef, err
	}

	return smsg.Cid(), nil
}
