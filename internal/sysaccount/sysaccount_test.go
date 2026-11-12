package sysaccount

import (
	"os"
	"os/user"
	"strconv"
	"testing"
)

func TestNumericIDsNeedNoLookup(t *testing.T) {
	if uid, err := UID("4242"); err != nil || uid != 4242 {
		t.Errorf("UID = %d, %v", uid, err)
	}
	if gid, err := GID("4243"); err != nil || gid != 4243 {
		t.Errorf("GID = %d, %v", gid, err)
	}
}

func TestUnknownNamesFail(t *testing.T) {
	if _, err := UID("skep-no-such-user"); err == nil {
		t.Error("unknown user accepted")
	}
	if _, err := GID("skep-no-such-group"); err == nil {
		t.Error("unknown group accepted")
	}
	if _, err := UIDs([]string{"0", "skep-no-such-user"}); err == nil {
		t.Error("a list with an unknown user accepted")
	}
}

func TestCurrentUserResolves(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Skip("no current user:", err)
	}
	uid, err := UID(me.Username)
	if err != nil || uid != uint32(os.Geteuid()) {
		t.Errorf("UID(%q) = %d, %v", me.Username, uid, err)
	}
	acct, err := Lookup(me.Username)
	if err != nil || acct.UID != uid {
		t.Errorf("Lookup = %+v, %v", acct, err)
	}
	uids, err := UIDs([]string{me.Username, strconv.Itoa(os.Geteuid()), "0"})
	if err != nil || len(uids) > 2 {
		t.Errorf("UIDs = %v, %v (duplicates must be dropped)", uids, err)
	}
}
