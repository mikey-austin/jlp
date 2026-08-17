package apitoken

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeRepo stores what a real one would, INCLUDING only the hash. A fake
// that kept the plaintext would let a "we never store the secret" test
// pass against an implementation that does — the exact shape of looseness
// that has hidden real bugs in this project before.
type fakeRepo struct {
	rows    map[string]storage.APITokenGrant // hash -> grant
	revoked map[string]bool                  // token id -> revoked
	touched []string
	names   map[string]string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{rows: map[string]storage.APITokenGrant{}, revoked: map[string]bool{}, names: map[string]string{}}
}

func (f *fakeRepo) Insert(_ context.Context, id string, identity learner.IdentityID, name, hash string, scopes []string) error {
	f.rows[hash] = storage.APITokenGrant{ID: id, Identity: identity, Scopes: scopes}
	f.names[id] = name
	return nil
}

func (f *fakeRepo) LookupLive(_ context.Context, hash string) (storage.APITokenGrant, error) {
	g, ok := f.rows[hash]
	if !ok || f.revoked[g.ID] {
		return storage.APITokenGrant{}, storage.ErrNotFound
	}
	return g, nil
}

func (f *fakeRepo) TouchLastUsed(_ context.Context, id string) error {
	f.touched = append(f.touched, id)
	return nil
}

func (f *fakeRepo) List(context.Context, learner.IdentityID) ([]storage.APIToken, error) {
	return nil, nil
}

func (f *fakeRepo) Revoke(_ context.Context, id string, _ learner.IdentityID) (bool, error) {
	if f.revoked[id] {
		return false, nil
	}
	f.revoked[id] = true
	return true, nil
}

func TestMintedTokenVerifies(t *testing.T) {
	repo := newFakeRepo()
	svc := New(repo)

	plaintext, err := svc.Mint(context.Background(), "mikey", "kobo", []string{ScopeVocabularyWrite})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !strings.HasPrefix(plaintext, tokenPrefix) {
		t.Errorf("token %q has no %q prefix, so it is unrecognisable in a log or a secret scanner", plaintext, tokenPrefix)
	}

	grant, err := svc.Verify(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if grant.Identity != "mikey" {
		t.Errorf("grant identity = %q, want mikey", grant.Identity)
	}
	if !HasScope(grant, ScopeVocabularyWrite) {
		t.Errorf("grant scopes = %v, want %s", grant.Scopes, ScopeVocabularyWrite)
	}
	if len(repo.touched) != 1 {
		t.Errorf("last-used recorded %d times, want 1 — an unused token must be visibly unused", len(repo.touched))
	}
}

// The plaintext must exist only in Mint's return value.
func TestPlaintextIsNeverStored(t *testing.T) {
	repo := newFakeRepo()
	svc := New(repo)

	plaintext, err := svc.Mint(context.Background(), "mikey", "kobo", nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	for hash, grant := range repo.rows {
		if strings.Contains(hash, plaintext) || hash == plaintext {
			t.Fatalf("the stored value %q contains the plaintext token — a database backup would yield working credentials", hash)
		}
		if strings.Contains(strings.Join(grant.Scopes, " "), plaintext) {
			t.Fatal("the plaintext leaked into the stored scopes")
		}
	}
	for _, name := range repo.names {
		if strings.Contains(name, plaintext) {
			t.Fatal("the plaintext leaked into the stored name")
		}
	}
}

func TestRevokedTokenStopsWorking(t *testing.T) {
	repo := newFakeRepo()
	svc := New(repo)

	plaintext, err := svc.Mint(context.Background(), "mikey", "kobo", []string{ScopeVocabularyWrite})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	grant, err := svc.Verify(context.Background(), plaintext)
	if err != nil {
		t.Fatalf("verify before revoke: %v", err)
	}

	if _, err := svc.Revoke(context.Background(), grant.ID, "mikey"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := svc.Verify(context.Background(), plaintext); !errors.Is(err, ErrInvalid) {
		t.Fatalf("verify after revoke = %v, want ErrInvalid — a revoked token must stop working immediately", err)
	}
}

func TestUnknownScopeIsRefusedAtMint(t *testing.T) {
	svc := New(newFakeRepo())
	if _, err := svc.Mint(context.Background(), "mikey", "kobo", []string{"vocabulary:wrote"}); !errors.Is(err, ErrUnknownScope) {
		t.Fatalf("mint with a typo'd scope = %v, want ErrUnknownScope — otherwise it mints a token granting nothing and nobody finds out until the client fails", err)
	}
}

func TestGarbageDoesNotVerify(t *testing.T) {
	repo := newFakeRepo()
	svc := New(repo)
	if _, err := svc.Mint(context.Background(), "mikey", "kobo", nil); err != nil {
		t.Fatalf("mint: %v", err)
	}
	for _, bad := range []string{"", "   ", "jlp_", "jlp_notarealtoken", "Bearer something"} {
		if _, err := svc.Verify(context.Background(), bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Verify(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

// Two mints must not collide, or revoking one would revoke the other.
func TestTokensAreUnique(t *testing.T) {
	repo := newFakeRepo()
	svc := New(repo)
	seen := map[string]bool{}
	for range 50 {
		p, err := svc.Mint(context.Background(), "mikey", "app", nil)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if seen[p] {
			t.Fatalf("mint produced a duplicate token")
		}
		seen[p] = true
	}
}

func TestMintRequiresAName(t *testing.T) {
	svc := New(newFakeRepo())
	if _, err := svc.Mint(context.Background(), "mikey", "  ", nil); err == nil {
		t.Fatal("minted a nameless token; it could only be revoked by recognising a hash")
	}
}
