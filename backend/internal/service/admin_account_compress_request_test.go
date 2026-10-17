//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type accountRepoStubForCompressRequest struct {
	mockAccountRepoForGemini
	account *Account
}

func (r *accountRepoStubForCompressRequest) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.account, nil
}

func (r *accountRepoStubForCompressRequest) Update(ctx context.Context, account *Account) error {
	r.account = account
	return nil
}

func TestAdminService_UpdateAccount_CompressRequestSwitch(t *testing.T) {
	account := &Account{
		ID:          41,
		Name:        "compress-request",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Extra:       map[string]any{},
	}
	repo := &accountRepoStubForCompressRequest{account: account}
	svc := &adminServiceImpl{accountRepo: repo}

	enabled := true
	updated, err := svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{
		CompressRequestEnabled: &enabled,
	})
	require.NoError(t, err)
	require.True(t, updated.CompressRequestEnabled)

	// Omitting the field preserves the stored switch.
	updated, err = svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{})
	require.NoError(t, err)
	require.True(t, updated.CompressRequestEnabled)

	disabled := false
	updated, err = svc.UpdateAccount(context.Background(), account.ID, &UpdateAccountInput{
		CompressRequestEnabled: &disabled,
	})
	require.NoError(t, err)
	require.False(t, updated.CompressRequestEnabled)
}

func TestBuildAccountForCreate_CompressRequestSwitchDefaultsOff(t *testing.T) {
	account, err := buildAccountForCreate(&CreateAccountInput{
		Name:        "default-off",
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{},
	}, map[string]any{})
	require.NoError(t, err)
	require.False(t, account.CompressRequestEnabled)

	enabled := true
	account, err = buildAccountForCreate(&CreateAccountInput{
		Name:                   "explicit-on",
		Platform:               PlatformOpenAI,
		Type:                   AccountTypeOAuth,
		Credentials:            map[string]any{},
		CompressRequestEnabled: &enabled,
	}, map[string]any{})
	require.NoError(t, err)
	require.True(t, account.CompressRequestEnabled)
}
