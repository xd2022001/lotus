package sealing

import (
	"context"
	"github.com/filecoin-project/specs-storage/storage"

	"github.com/filecoin-project/specs-actors/actors/crypto"

	"github.com/filecoin-project/go-statemachine"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	"github.com/filecoin-project/specs-actors/actors/builtin/miner"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build"
	"github.com/filecoin-project/lotus/chain/actors"
	"github.com/filecoin-project/lotus/chain/types"
)

func (m *Sealing) handlePacking(ctx statemachine.Context, sector SectorInfo) error {
	log.Infow("performing filling up rest of the sector...", "sector", sector.SectorID)

	var allocated abi.UnpaddedPieceSize
	for _, piece := range sector.Pieces {
		allocated += piece.Size
	}

	ubytes := abi.PaddedPieceSize(m.sealer.SectorSize()).Unpadded()

	if allocated > ubytes {
		return xerrors.Errorf("too much data in sector: %d > %d", allocated, ubytes)
	}

	fillerSizes, err := fillersFromRem(ubytes - allocated)
	if err != nil {
		return err
	}

	if len(fillerSizes) > 0 {
		log.Warnf("Creating %d filler pieces for sector %d", len(fillerSizes), sector.SectorID)
	}

	pieces, err := m.pledgeSector(ctx.Context(), m.minerSector(sector.SectorID), sector.existingPieces(), fillerSizes...)
	if err != nil {
		return xerrors.Errorf("filling up the sector (%v): %w", fillerSizes, err)
	}

	return ctx.Send(SectorPacked{Pieces: pieces})
}

func (m *Sealing) handlePreCommit1(ctx statemachine.Context, sector SectorInfo) error {
	if err := checkPieces(ctx.Context(), sector, m.api); err != nil { // Sanity check state
		switch err.(type) {
		case *ErrApi:
			log.Errorf("handlePreCommit1: api error, not proceeding: %+v", err)
			return nil
		case *ErrInvalidDeals:
			return ctx.Send(SectorPackingFailed{xerrors.Errorf("invalid deals in sector: %w", err)})
		case *ErrExpiredDeals: // Probably not much we can do here, maybe re-pack the sector?
			return ctx.Send(SectorPackingFailed{xerrors.Errorf("expired deals in sector: %w", err)})
		default:
			return xerrors.Errorf("checkPieces sanity check error: %w", err)
		}
	}

	log.Infow("performing sector replication...", "sector", sector.SectorID)
	ticket, err := m.tktFn(ctx.Context())
	if err != nil {
		return ctx.Send(SectorSealPreCommitFailed{xerrors.Errorf("getting ticket failed: %w", err)})
	}

	pc1o, err := m.sealer.SealPreCommit1(ctx.Context(), m.minerSector(sector.SectorID), ticket.Value, sector.pieceInfos())
	if err != nil {
		return ctx.Send(SectorSealPreCommitFailed{xerrors.Errorf("seal pre commit(1) failed: %w", err)})
	}

	return ctx.Send(SectorPreCommit1{
		PreCommit1Out: pc1o,
		Ticket:        *ticket,
	})
}

func (m *Sealing) handlePreCommit2(ctx statemachine.Context, sector SectorInfo) error {
	cids, err := m.sealer.SealPreCommit2(ctx.Context(), m.minerSector(sector.SectorID), sector.PreCommit1Out)
	if err != nil {
		return ctx.Send(SectorSealPreCommitFailed{xerrors.Errorf("seal pre commit(2) failed: %w", err)})
	}

	return ctx.Send(SectorPreCommit2{
		Unsealed: cids.Unsealed,
		Sealed:   cids.Sealed,
	})
}

func (m *Sealing) handlePreCommitting(ctx statemachine.Context, sector SectorInfo) error {
	if err := checkPrecommit(ctx.Context(), m.maddr, sector, m.api); err != nil {
		switch err.(type) {
		case *ErrApi:
			log.Errorf("handlePreCommitting: api error, not proceeding: %+v", err)
			return nil
		case *ErrBadCommD: // TODO: Should this just back to packing? (not really needed since handlePreCommit1 will do that too)
			return ctx.Send(SectorSealPreCommitFailed{xerrors.Errorf("bad CommD error: %w", err)})
		case *ErrExpiredTicket:
			return ctx.Send(SectorSealPreCommitFailed{xerrors.Errorf("ticket expired: %w", err)})
		default:
			return xerrors.Errorf("checkPrecommit sanity check error: %w", err)
		}
	}

	params := &miner.SectorPreCommitInfo{
		Expiration:      10000000, // TODO: implement
		SectorNumber:    sector.SectorID,
		RegisteredProof: sector.SectorType,

		SealedCID:     *sector.CommR,
		SealRandEpoch: sector.Ticket.Epoch,
		DealIDs:       sector.deals(),
	}
	enc, aerr := actors.SerializeParams(params)
	if aerr != nil {
		return ctx.Send(SectorChainPreCommitFailed{xerrors.Errorf("could not serialize commit sector parameters: %w", aerr)})
	}

	msg := &types.Message{
		To:       m.maddr,
		From:     m.worker,
		Method:   builtin.MethodsMiner.PreCommitSector,
		Params:   enc,
		Value:    types.NewInt(0), // TODO: need to ensure sufficient collateral
		GasLimit: 1000000,         /* i dont know help */
		GasPrice: types.NewInt(1),
	}

	log.Info("submitting precommit for sector: ", sector.SectorID)
	smsg, err := m.api.MpoolPushMessage(ctx.Context(), msg)
	if err != nil {
		return ctx.Send(SectorChainPreCommitFailed{xerrors.Errorf("pushing message to mpool: %w", err)})
	}

	return ctx.Send(SectorPreCommitted{Message: smsg.Cid()})
}

func (m *Sealing) handleWaitSeed(ctx statemachine.Context, sector SectorInfo) error {
	// would be ideal to just use the events.Called handler, but it wouldnt be able to handle individual message timeouts
	log.Info("Sector precommitted: ", sector.SectorID)
	mw, err := m.api.StateWaitMsg(ctx.Context(), *sector.PreCommitMessage)
	if err != nil {
		return ctx.Send(SectorChainPreCommitFailed{err})
	}

	if mw.Receipt.ExitCode != 0 {
		log.Error("sector precommit failed: ", mw.Receipt.ExitCode)
		err := xerrors.Errorf("sector precommit failed: %d", mw.Receipt.ExitCode)
		return ctx.Send(SectorChainPreCommitFailed{err})
	}
	log.Info("precommit message landed on chain: ", sector.SectorID)

	pci, err := m.api.StateSectorPreCommitInfo(ctx.Context(), m.maddr, sector.SectorID, mw.TipSet.Key())
	if err != nil {
		return xerrors.Errorf("getting precommit info: %w", err)
	}

	randHeight := pci.PreCommitEpoch + miner.PreCommitChallengeDelay
	log.Infof("precommit for sector %d made it on chain, will start proof computation at height %d", sector.SectorID, randHeight)

	err = m.events.ChainAt(func(ectx context.Context, ts *types.TipSet, curH abi.ChainEpoch) error {
		rand, err := m.api.ChainGetRandomness(ectx, ts.Key(), crypto.DomainSeparationTag_InteractiveSealChallengeSeed, randHeight, nil)
		if err != nil {
			err = xerrors.Errorf("failed to get randomness for computing seal proof: %w", err)

			ctx.Send(SectorFatalError{error: err})
			return err
		}

		ctx.Send(SectorSeedReady{Seed: api.SealSeed{
			Epoch: randHeight,
			Value: abi.InteractiveSealRandomness(rand),
		}})

		return nil
	}, func(ctx context.Context, ts *types.TipSet) error {
		log.Warn("revert in interactive commit sector step")
		// TODO: need to cancel running process and restart...
		return nil
	}, build.InteractivePoRepConfidence, randHeight)
	if err != nil {
		log.Warn("waitForPreCommitMessage ChainAt errored: ", err)
	}

	return nil
}

func (m *Sealing) handleCommitting(ctx statemachine.Context, sector SectorInfo) error {
	log.Info("scheduling seal proof computation...")

	log.Infof("KOMIT %d %x(%d); %x(%d); %v; r:%x; d:%x", sector.SectorID, sector.Ticket.Value, sector.Ticket.Epoch, sector.Seed.Value, sector.Seed.Epoch, sector.pieceInfos(), sector.CommR, sector.CommD)

	cids := storage.SectorCids{
		Unsealed: *sector.CommD,
		Sealed:   *sector.CommR,
	}
	c2in, err := m.sealer.SealCommit1(ctx.Context(), m.minerSector(sector.SectorID), sector.Ticket.Value, sector.Seed.Value, sector.pieceInfos(), cids)
	if err != nil {
		return ctx.Send(SectorComputeProofFailed{xerrors.Errorf("computing seal proof failed: %w", err)})
	}

	proof, err := m.sealer.SealCommit2(ctx.Context(), m.minerSector(sector.SectorID), c2in)
	if err != nil {
		return ctx.Send(SectorComputeProofFailed{xerrors.Errorf("computing seal proof failed: %w", err)})
	}

	if err := m.checkCommit(ctx.Context(), sector, proof); err != nil {
		return ctx.Send(SectorCommitFailed{xerrors.Errorf("commit check error: %w", err)})
	}

	// TODO: Consider splitting states and persist proof for faster recovery

	params := &miner.ProveCommitSectorParams{
		SectorNumber: sector.SectorID,
		Proof:        proof,
	}

	enc, aerr := actors.SerializeParams(params)
	if aerr != nil {
		return ctx.Send(SectorCommitFailed{xerrors.Errorf("could not serialize commit sector parameters: %w", aerr)})
	}

	msg := &types.Message{
		To:       m.maddr,
		From:     m.worker,
		Method:   builtin.MethodsMiner.ProveCommitSector,
		Params:   enc,
		Value:    types.NewInt(0), // TODO: need to ensure sufficient collateral
		GasLimit: 1000000,         /* i dont know help */
		GasPrice: types.NewInt(1),
	}

	// TODO: check seed / ticket are up to date

	smsg, err := m.api.MpoolPushMessage(ctx.Context(), msg)
	if err != nil {
		return ctx.Send(SectorCommitFailed{xerrors.Errorf("pushing message to mpool: %w", err)})
	}

	return ctx.Send(SectorCommitted{
		Proof:   proof,
		Message: smsg.Cid(),
	})
}

func (m *Sealing) handleCommitWait(ctx statemachine.Context, sector SectorInfo) error {
	if sector.CommitMessage == nil {
		log.Errorf("sector %d entered commit wait state without a message cid", sector.SectorID)
		return ctx.Send(SectorCommitFailed{xerrors.Errorf("entered commit wait with no commit cid")})
	}

	mw, err := m.api.StateWaitMsg(ctx.Context(), *sector.CommitMessage)
	if err != nil {
		return ctx.Send(SectorCommitFailed{xerrors.Errorf("failed to wait for porep inclusion: %w", err)})
	}

	if mw.Receipt.ExitCode != 0 {
		return ctx.Send(SectorCommitFailed{xerrors.Errorf("submitting sector proof failed (exit=%d, msg=%s) (t:%x; s:%x(%d); p:%x)", mw.Receipt.ExitCode, sector.CommitMessage, sector.Ticket.Value, sector.Seed.Value, sector.Seed.Epoch, sector.Proof)})
	}

	return ctx.Send(SectorProving{})
}

func (m *Sealing) handleFinalizeSector(ctx statemachine.Context, sector SectorInfo) error {
	// TODO: Maybe wait for some finality

	if err := m.sealer.FinalizeSector(ctx.Context(), m.minerSector(sector.SectorID)); err != nil {
		return ctx.Send(SectorFinalizeFailed{xerrors.Errorf("finalize sector: %w", err)})
	}

	return ctx.Send(SectorFinalized{})
}

func (m *Sealing) handleFaulty(ctx statemachine.Context, sector SectorInfo) error {
	// TODO: check if the fault has already been reported, and that this sector is even valid

	// TODO: coalesce faulty sector reporting
	bf := abi.NewBitField()
	bf.Set(uint64(sector.SectorID))

	enc, aerr := actors.SerializeParams(&miner.DeclareTemporaryFaultsParams{
		SectorNumbers: bf,
		Duration:      99999999, // TODO: This is very unlikely to be the correct number
	})
	if aerr != nil {
		return xerrors.Errorf("failed to serialize declare fault params: %w", aerr)
	}

	msg := &types.Message{
		To:       m.maddr,
		From:     m.worker,
		Method:   builtin.MethodsMiner.DeclareTemporaryFaults,
		Params:   enc,
		Value:    types.NewInt(0), // TODO: need to ensure sufficient collateral
		GasLimit: 1000000,         /* i dont know help */
		GasPrice: types.NewInt(1),
	}

	smsg, err := m.api.MpoolPushMessage(ctx.Context(), msg)
	if err != nil {
		return xerrors.Errorf("failed to push declare faults message to network: %w", err)
	}

	return ctx.Send(SectorFaultReported{reportMsg: smsg.Cid()})
}

func (m *Sealing) handleFaultReported(ctx statemachine.Context, sector SectorInfo) error {
	if sector.FaultReportMsg == nil {
		return xerrors.Errorf("entered fault reported state without a FaultReportMsg cid")
	}

	mw, err := m.api.StateWaitMsg(ctx.Context(), *sector.FaultReportMsg)
	if err != nil {
		return xerrors.Errorf("failed to wait for fault declaration: %w", err)
	}

	if mw.Receipt.ExitCode != 0 {
		log.Errorf("UNHANDLED: declaring sector fault failed (exit=%d, msg=%s) (id: %d)", mw.Receipt.ExitCode, *sector.FaultReportMsg, sector.SectorID)
		return xerrors.Errorf("UNHANDLED: submitting fault declaration failed (exit %d)", mw.Receipt.ExitCode)
	}

	return ctx.Send(SectorFaultedFinal{})
}
