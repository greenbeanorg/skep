package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/internal/apidb"
	"github.com/greenbeanorg/skep/internal/auth"
	"github.com/greenbeanorg/skep/pkg/models"
)

var noQuorum = &models.Error{Code: auth.CodeNoQuorum}

// waitQuorum waits until the copy reports quorum (want) or not.
func (f *fixture) waitQuorum(t *testing.T, want bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); f.writer.Quorum() != want; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("quorum did not become %v", want)
		}
	}
}

// D18 and V6: without quorum the password is checked first, exactly as
// with quorum; an enabled administrator gets a degraded session of at most
// one hour, anyone else with the right password no_quorum, a wrong
// password or an unknown user invalid_credentials. A user change is
// no_quorum (D17).
func TestLoginWithoutQuorum(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.create(t, "andy", auth.RoleAdmin)
	f.create(t, "olga", auth.RoleOperator)
	f.node.Stop()
	f.waitQuorum(t, false)

	res, err := f.authn.Login(ctx, loginReq("andy", goodPassword, auth.BearerSession))
	if err != nil || !res.Session.Degraded || res.Session.ExpiresAt.After(f.clock.now().Add(time.Hour)) || res.Session.CredentialEpoch != 1 {
		t.Fatalf("administrator without quorum: %+v %v", res.Session, err)
	}
	if id, err := f.authn.Authenticate(ctx, res.Token, auth.BearerSession); err != nil || !id.Session.Degraded {
		t.Fatalf("the degraded session: %+v %v", id.Session, err)
	}
	if _, err := f.authn.Login(ctx, loginReq("olga", goodPassword, auth.BearerSession)); !errors.Is(err, noQuorum) {
		t.Fatalf("operator with the right password: %v", err)
	}
	for _, name := range []string{"olga", "andy", "nobody"} {
		if _, err := f.authn.Login(ctx, loginReq(name, "a wrong passphrase here", auth.BearerSession)); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("%s with a wrong password: %v", name, err)
		}
	}
	_, err = f.users.Create(ctx, local, auth.NewUser{Username: "nora", Password: goodPassword})
	var me *models.Error
	if !errors.As(err, &me) || me.Code != auth.CodeNoQuorum || me.Details["outcome"] != auth.OutcomeNotWritten {
		t.Fatalf("a create without quorum: %v", err)
	}
	// Quorum back: a normal session again.
	f.node.Restart(t)
	f.waitQuorum(t, true)
	if res, err := f.authn.Login(ctx, loginReq("olga", goodPassword, auth.BearerSession)); err != nil || res.Session.Degraded ||
		!res.Session.ExpiresAt.Equal(f.clock.now().Add(auth.DefaultAbsoluteTimeout)) {
		t.Fatalf("with quorum: %+v %v", res.Session, err)
	}
}

// D18: an empty copy without quorum answers no_quorum, not setup_required.
func TestEmptyCopyWithoutQuorum(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.authn.Login(ctx, loginReq("andy", goodPassword, auth.BearerSession)); !errors.Is(err, auth.ErrSetupRequired) {
		t.Fatalf("empty copy with quorum: %v", err)
	}
	f.node.Stop()
	f.waitQuorum(t, false)
	if _, err := f.authn.Login(ctx, loginReq("andy", goodPassword, auth.BearerSession)); !errors.Is(err, noQuorum) {
		t.Fatalf("empty copy without quorum: %v", err)
	}
}

// D15: a login that read the user before a password change was applied
// stores the old epoch; its session is refused on the first request.
func TestSessionCreatedWhileAPasswordChangeIsApplied(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	read, _ := f.store.UserByID(ctx, u.ID) // what the racing login read
	if err := f.users.SetPassword(ctx, local, u.ID, "a new and different passphrase"); err != nil {
		t.Fatal(err)
	}
	token, _ := auth.NewToken()
	now := f.clock.now()
	if err := apidb.NewSessionStore(f.db).CreateSession(ctx, auth.Session{TokenHash: auth.HashToken(token), UserID: u.ID, Kind: auth.BearerSession,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour), CredentialEpoch: read.CredentialEpoch}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.authn.Authenticate(ctx, token, auth.BearerSession); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("a session with the old epoch was accepted: %v", err)
	}
	// A login now gets the new epoch, and works.
	res, err := f.authn.Login(ctx, loginReq("andy", "a new and different passphrase", auth.BearerSession))
	if err != nil || res.Session.CredentialEpoch != 2 {
		t.Fatalf("login after the change: %+v %v", res.Session, err)
	}
	if _, err := f.authn.Authenticate(ctx, res.Token, auth.BearerSession); err != nil {
		t.Fatal(err)
	}
}

// D19: the hash upgrade at login is a user.rehash through the cluster; it
// keeps the epoch and the sessions.
func TestRehashGoesThroughTheCluster(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	u := f.create(t, "andy", auth.RoleAdmin)
	stronger, err := auth.NewHasher(auth.Params{MemoryKiB: auth.MinParams.MemoryKiB, Passes: 3, Lanes: 1}, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opts := f.authnOptions()
	opts.Hasher = stronger
	authn, err := auth.NewAuthenticator(opts)
	if err != nil {
		t.Fatal(err)
	}
	res, err := authn.Login(ctx, loginReq("andy", goodPassword, auth.BearerSession))
	if err != nil {
		t.Fatal(err)
	}
	after, _ := f.store.UserByID(ctx, u.ID)
	if after.PasswordHash == u.PasswordHash || after.CredentialEpoch != 1 {
		t.Fatalf("rehash: epoch %d, hash changed %v", after.CredentialEpoch, after.PasswordHash != u.PasswordHash)
	}
	if _, err := authn.Authenticate(ctx, res.Token, auth.BearerSession); err != nil {
		t.Fatalf("the session ended with the rehash: %v", err)
	}
}
