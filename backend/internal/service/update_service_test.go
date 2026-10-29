//go:build unit

package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	checksumData   []byte
	checksumURL    string
	requestedRepo  string
	release        *GitHubRelease
	recentReleases []*GitHubRelease
	recentErr      error
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(_ context.Context, repo string) (*GitHubRelease, error) {
	s.requestedRepo = repo
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(_ context.Context, repo string, _ int) ([]*GitHubRelease, error) {
	s.requestedRepo = repo
	return s.recentReleases, s.recentErr
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(_ context.Context, url string) ([]byte, error) {
	s.checksumURL = url
	return s.checksumData, nil
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.1.132",
				Name:    "v0.1.132",
			},
		},
		"0.1.132",
		"release",
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func newRollbackTestService(current string, releases []*GitHubRelease) *UpdateService {
	return NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentReleases: releases},
		current,
		"release",
	)
}

func TestUpdateServiceListRollbackVersionsFiltersAndCaps(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148", PublishedAt: "2026-07-09T00:00:00Z"},                       // newer than current: excluded
		{TagName: "v0.1.147", PublishedAt: "2026-07-08T00:00:00Z"},                       // current: excluded
		{TagName: "v0.1.146-rc1", PublishedAt: "2026-07-07T12:00:00Z", Prerelease: true}, // prerelease: excluded
		{TagName: "v0.1.146", PublishedAt: "2026-07-07T00:00:00Z"},
		{TagName: "v0.1.145", PublishedAt: "2026-07-06T00:00:00Z", Draft: true}, // draft: excluded
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"},
		{TagName: "v0.1.144", PublishedAt: "2026-07-05T00:00:00Z"}, // duplicate: excluded
		{TagName: "v0.1.143", PublishedAt: "2026-07-04T00:00:00Z"},
		{TagName: "v0.1.142", PublishedAt: "2026-07-03T00:00:00Z"}, // beyond cap of 3: excluded
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.144", versions[1].Version)
	require.Equal(t, "0.1.143", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsSortsUnorderedInput(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.144"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "0.1.146", versions[0].Version)
	require.Equal(t, "0.1.145", versions[1].Version)
	require.Equal(t, "0.1.144", versions[2].Version)
}

func TestUpdateServiceListRollbackVersionsEmptyWhenNoneOlder(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.148"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Empty(t, versions)
}

func TestUpdateServiceListRollbackVersionsPropagatesFetchError(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{recentErr: errors.New("github unavailable")},
		"0.1.147",
		"release",
	)

	_, err := svc.ListRollbackVersions(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "github unavailable")
}

func TestUpdateServiceRollbackToVersionRejectsDisallowedTargets(t *testing.T) {
	releases := []*GitHubRelease{
		{TagName: "v0.1.148"},
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
		{TagName: "v0.1.145"},
		{TagName: "v0.1.144"},
		{TagName: "v0.1.143"},
		{TagName: "v0.1.142"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	for _, target := range []string{
		"",         // empty
		"0.1.147",  // current version
		"v0.1.147", // current version with prefix
		"0.1.148",  // newer than current
		"0.1.142",  // older than the 3 most recent
		"9.9.9",    // nonexistent
	} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackToVersionAcceptsVPrefix(t *testing.T) {
	// No platform asset in the release: the target passes the allowlist check
	// and fails later at asset lookup, proving the version itself was accepted.
	releases := []*GitHubRelease{
		{TagName: "v0.1.147"},
		{TagName: "v0.1.146"},
	}
	svc := newRollbackTestService("0.1.147", releases)

	err := svc.RollbackToVersion(context.Background(), "v0.1.146")

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrRollbackVersionNotAllowed)
	require.Contains(t, err.Error(), "no compatible release found")
}

func TestCompareVersionsForkRevision(t *testing.T) {
	for _, tc := range []struct {
		current, latest string
		want            int
	}{
		{"z0.2.18-9", "z0.2.18-10", -1},
		{"z0.2.18-10", "z0.2.18-9", 1},
		{"z0.2.18-10", "z0.2.18-10", 0},
		{"z0.2.18-100", "z0.2.19-1", -1},
		{"v0.2.18", "z0.2.18-1", -1},
		{"0.2.19", "z0.2.18-1", 1},
		{"traffic-stat-578c32412b22", "z0.2.18-1", -1},
		{"vtraffic-stat-578c32412b22", "z0.2.18-1", -1},
		{"dev", "z0.2.18-1", 0},
		{"z0.2.18-1", "garbage", 0},
	} {
		require.Equal(t, tc.want, compareVersions(tc.current, tc.latest), "%s vs %s", tc.current, tc.latest)
	}
}

func TestParseReleaseVersionRejectsMalformedVersions(t *testing.T) {
	for _, version := range []string{"", "z0.2.18", "z0.2.18-0", "z0.2.18-01", "z0.2.18-rc1", "z0.2.18-1-extra", "0.2", "z0.2.18-18446744073709551616"} {
		_, valid := parseVersion(version)
		require.False(t, valid, version)
	}
}

func TestUpdateServiceForkLatestAndCacheMigration(t *testing.T) {
	cache := &updateServiceCacheStub{data: `{"latest":"99.0.0","release_info":{},"timestamp":` + strconv.FormatInt(time.Now().Unix(), 10) + `}`}
	client := &updateServiceGitHubClientStub{release: &GitHubRelease{TagName: "z0.2.18-10"}}
	svc := NewUpdateService(cache, client, "z0.2.18-9", "release")
	info, err := svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.Equal(t, "Zmmfly/sub2api", client.requestedRepo)
	require.Equal(t, "z0.2.18-10", info.LatestVersion)
	require.True(t, info.HasUpdate)
	require.False(t, info.Cached)
	info, err = svc.CheckUpdate(context.Background(), false)
	require.NoError(t, err)
	require.True(t, info.Cached)
	require.True(t, info.HasUpdate)
}

func TestUpdateServiceForkRollbackOrder(t *testing.T) {
	svc := newRollbackTestService("z0.2.18-11", []*GitHubRelease{
		{TagName: "z0.2.18-9"}, {TagName: "z0.2.18-10"}, {TagName: "z0.2.18-8"},
		{TagName: "vtraffic-stat-578c32412b22"}, {TagName: "z0.2.18-12"}, {TagName: "invalid"},
	})
	versions, err := svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Len(t, versions, 3)
	require.Equal(t, "z0.2.18-10", versions[0].Version)
	require.Equal(t, "z0.2.18-9", versions[1].Version)
	require.Equal(t, "z0.2.18-8", versions[2].Version)
	err = svc.RollbackToVersion(context.Background(), "z0.2.18-10")
	require.ErrorContains(t, err, "no compatible release found")
}

func TestUpdateServiceForkAssetValidation(t *testing.T) {
	base := "https://github.com/Zmmfly/sub2api/releases/download/z0.2.18-1/"
	require.NoError(t, validateDownloadURL(base + "checksums.txt"))
	for _, raw := range []string{
		"https://github.com/Wei-Shaw/sub2api/releases/download/v0.2.18/checksums.txt",
		"https://github.com/Zmmfly/sub2api-other/releases/download/z0.2.18-1/checksums.txt",
		"https://evil.github.com/Zmmfly/sub2api/releases/download/z0.2.18-1/checksums.txt",
		"http://github.com/Zmmfly/sub2api/releases/download/z0.2.18-1/checksums.txt",
	} {
		require.Error(t, validateDownloadURL(raw), raw)
	}
	svc := NewUpdateService(nil, nil, "z0.2.18-1", "release")
	assets := []Asset{{Name: "sub2api_z0.2.18-2_" + svc.getArchiveName() + ".tar.gz", DownloadURL: base + "archive.tar.gz"}}
	require.ErrorContains(t, svc.applyReleaseAssets(context.Background(), assets), "missing checksums.txt")
	assets = append(assets, Asset{Name: "checksums.txt", DownloadURL: "https://github.com/Wei-Shaw/sub2api/releases/download/v0.2.18/checksums.txt"})
	require.ErrorContains(t, svc.applyReleaseAssets(context.Background(), assets), "invalid checksum URL")
}

func TestUpdateServiceVerifiesForkArchiveChecksum(t *testing.T) {
	archiveName := "sub2api_z0.2.18-10_linux_amd64.tar.gz"
	archivePath := filepath.Join(t.TempDir(), archiveName)
	content := []byte("archive fixture")
	require.NoError(t, os.WriteFile(archivePath, content, 0600))
	client := &updateServiceGitHubClientStub{
		checksumData: []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(content), archiveName)),
	}
	svc := NewUpdateService(nil, client, "z0.2.18-9", "release")
	checksumURL := "https://github.com/Zmmfly/sub2api/releases/download/z0.2.18-10/checksums.txt"
	require.NoError(t, svc.verifyChecksum(context.Background(), archivePath, checksumURL))
	require.Equal(t, checksumURL, client.checksumURL)
	require.NoError(t, os.WriteFile(archivePath, []byte("tampered"), 0600))
	require.ErrorContains(t, svc.verifyChecksum(context.Background(), archivePath, checksumURL), "checksum mismatch")
}

func TestUpdateServiceRejectsNonCanonicalLatest(t *testing.T) {
	for _, tag := range []string{"v0.2.19", "traffic-stat-578c32412b22", "z0.2.18-0", "z0.2.18-1-rc1"} {
		svc := NewUpdateService(&updateServiceCacheStub{}, &updateServiceGitHubClientStub{
			release: &GitHubRelease{TagName: tag},
		}, "z0.2.18-1", "release")
		info, err := svc.CheckUpdate(context.Background(), true)
		require.NoError(t, err)
		require.False(t, info.HasUpdate, tag)
		require.Contains(t, info.Warning, "unsupported release version")
	}
}
