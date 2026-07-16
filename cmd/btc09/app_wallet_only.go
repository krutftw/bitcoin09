package main

import (
	"context"

	"github.com/krutftw/bitcoin09/desktop"
)

// walletOnlyAppService deliberately forwards every wallet capability except
// desktop.MinerService. Store and mobile editions therefore cannot reach the
// on-device miner even though they share the reviewed wallet implementation.
type walletOnlyAppService struct {
	inner *appService
}

func walletOnlyStatus(status desktop.Status) desktop.Status {
	status.MiningAvailable = false
	return status
}

func newWalletOnlyAppService(inner *appService) *walletOnlyAppService {
	return &walletOnlyAppService{inner: inner}
}

func (s *walletOnlyAppService) Status(ctx context.Context) (desktop.Status, error) {
	status, err := s.inner.Status(ctx)
	return walletOnlyStatus(status), err
}

func (s *walletOnlyAppService) CreateWallet(ctx context.Context) (desktop.Status, error) {
	status, err := s.inner.CreateWallet(ctx)
	return walletOnlyStatus(status), err
}

func (s *walletOnlyAppService) NewAddress(ctx context.Context) (desktop.AddressResult, error) {
	return s.inner.NewAddress(ctx)
}

func (s *walletOnlyAppService) Backup(ctx context.Context, destination string) (desktop.BackupResult, error) {
	return s.inner.Backup(ctx, destination)
}

func (s *walletOnlyAppService) PreviewSend(ctx context.Context, request desktop.SendRequest) (desktop.SendPreview, error) {
	return s.inner.PreviewSend(ctx, request)
}

func (s *walletOnlyAppService) ConfirmSend(ctx context.Context, pendingID string) (desktop.SendResult, error) {
	return s.inner.ConfirmSend(ctx, pendingID)
}

func (s *walletOnlyAppService) Activity(ctx context.Context) (desktop.ActivityResult, error) {
	return s.inner.Activity(ctx)
}

func (s *walletOnlyAppService) PreviewMaxSend(ctx context.Context, request desktop.MaxSendRequest) (desktop.SendPreview, error) {
	return s.inner.PreviewMaxSend(ctx, request)
}

func (s *walletOnlyAppService) PreviewCleanup(ctx context.Context, request desktop.CleanupRequest) (desktop.CleanupPreview, error) {
	return s.inner.PreviewCleanup(ctx, request)
}

func (s *walletOnlyAppService) ConfirmCleanup(ctx context.Context, pendingID string) (desktop.SendResult, error) {
	return s.inner.ConfirmCleanup(ctx, pendingID)
}

func (s *walletOnlyAppService) CancelPreview(ctx context.Context, pendingID string) error {
	return s.inner.CancelPreview(ctx, pendingID)
}

func (s *walletOnlyAppService) CreateRecoveryWallet(ctx context.Context, request desktop.RecoveryWalletCreateRequest) (desktop.RecoveryWalletCreateResult, error) {
	result, err := s.inner.CreateRecoveryWallet(ctx, request)
	result.Status = walletOnlyStatus(result.Status)
	return result, err
}

func (s *walletOnlyAppService) RestoreRecoveryWallet(ctx context.Context, request desktop.RecoveryWalletRestoreRequest) (desktop.Status, error) {
	status, err := s.inner.RestoreRecoveryWallet(ctx, request)
	return walletOnlyStatus(status), err
}

func (s *walletOnlyAppService) UnlockRecoveryWallet(ctx context.Context, request desktop.RecoveryWalletUnlockRequest) (desktop.Status, error) {
	status, err := s.inner.UnlockRecoveryWallet(ctx, request)
	return walletOnlyStatus(status), err
}

func (s *walletOnlyAppService) RecoveryPhrase(ctx context.Context, request desktop.RecoveryWalletUnlockRequest) (desktop.RecoveryPhraseResult, error) {
	return s.inner.RecoveryPhrase(ctx, request)
}

var (
	_ desktop.Service               = (*walletOnlyAppService)(nil)
	_ desktop.WalletFeaturesService = (*walletOnlyAppService)(nil)
	_ desktop.PreviewCancelService  = (*walletOnlyAppService)(nil)
	_ desktop.RecoveryWalletService = (*walletOnlyAppService)(nil)
)
