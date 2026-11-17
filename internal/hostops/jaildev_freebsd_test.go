//go:build freebsd

package hostops

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/greenbeanorg/skep/pkg/models"
)

// TestNativeDevGuardRefusesLeakyRuleset triggers the /dev guard on a real
// jail (amendment i; Decision 16, "Jail devfs"). Ruleset 6001 is checked
// line by line before every start, so no leaky ruleset can reach jail -c
// through the agent; here the test changes the expected rules to 6001's
// own plus one harmless extra, "path full unhide". The real StartJail adds
// that ruleset, creates the jail with a DHCP NIC on bridge0, and the real
// guard, with the real allow-list, must find /dev/full and remove the jail
// before /etc/rc runs, with its epair. The ruleset is removed afterwards
// whatever happens.
//
// It needs root on FreeBSD, if_epair, bridge0, an empty ruleset 6001, and
// SKEP_GUARD_TEMPLATE: the ID of a finished template in skeptest/skep.
func TestNativeDevGuardRefusesLeakyRuleset(t *testing.T) {
	tid := os.Getenv("SKEP_GUARD_TEMPLATE")
	if os.Geteuid() != 0 || tid == "" {
		t.Skip("needs root and SKEP_GUARD_TEMPLATE")
	}
	ctx := context.Background()
	conf := Confinement{Pools: []string{"skeptest"}, DatasetRoot: "skep", Bridges: []string{"bridge0"},
		ManagementInterfaces: []string{"vtnet0"}, JailRunDir: t.TempDir()}
	h := newHost(conf, execRunner{timeout: 30 * time.Second}, kernel{}, slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if rules, err := h.devfsRules(ctx); err != nil || len(rules) != 0 {
		t.Fatalf("ruleset 6001 must be empty before this test: %q, %v", rules, err)
	}
	saved := jailDHCPRules
	jailDHCPRules = append(slices.Clone(saved), "700 path full unhide")
	t.Cleanup(func() {
		jailDHCPRules = saved
		if _, err := h.run.Run(context.Background(), devfsBinary, "rule", "-s", "6001", "delset"); err != nil {
			t.Errorf("remove ruleset 6001: %v", err)
		}
		if rules, err := h.devfsRules(context.Background()); err != nil || len(rules) != 0 {
			t.Errorf("ruleset 6001 is still there: %q, %v", rules, err)
		}
	})

	cid := testUUID(t)
	if _, err := h.CreateJailRoot(ctx, "skeptest", tid, cid); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := h.DestroyJailRoot(context.Background(), "skeptest", cid); err != nil {
			t.Errorf("destroy the jail root: %v", err)
		}
	})
	spec := JailSpec{Pool: "skeptest", ContainerID: cid, Hostname: "guardtest", StartTimeout: 120 * time.Second,
		StopTimeout: 10 * time.Second, NICs: []JailNIC{{Slot: 0, Bridge: "bridge0", MAC: "02:00:5e:70:00:01", DHCP: true}}}
	t.Logf("container %s", cid)

	_, err := h.StartJail(ctx, spec)
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodeContainerStartFailed {
		t.Fatalf("StartJail = %v, want container_start_failed from the guard", err)
	}
	t.Logf("refusal: %s", me.Message)
	if extra, _ := me.Details["extra_devices"].([]string); !slices.Equal(extra, []string{"full"}) {
		t.Fatalf("extra_devices = %v, want [full]", me.Details["extra_devices"])
	}

	name, _ := JailName(cid)
	if exists, err := h.jailExists(ctx, name); err != nil || exists {
		t.Fatalf("the refused jail is still there (%v)", err)
	}
	if ifs, err := h.SkepInterfaces(ctx); err != nil {
		t.Fatal(err)
	} else {
		for _, i := range ifs {
			if i.OwnerID == cid {
				t.Fatalf("the refused jail's epair %s is still there", i.Name)
			}
		}
	}
	jif, _ := JailInterfaceName(cid, 0)
	if exists, err := h.interfaceExists(ctx, jif); err != nil || exists {
		t.Fatalf("the jail end %s is still on the host (%v)", jif, err)
	}
	// /etc/rc never ran: Skep appends its output to console.log, and
	// jail(8) writes exec.* output there; there was none.
	logPath, _ := conf.JailRunPath(cid, JailConsoleLog)
	if b, err := os.ReadFile(logPath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		t.Fatalf("console.log of the refused jail is not empty:\n%s", b)
	}
	// The removal used a jail.conf without exec.stop.
	confPath, _ := conf.JailRunPath(cid, JailConfFile)
	if b, err := os.ReadFile(confPath); err != nil || strings.Contains(string(b), "exec.stop") || strings.Contains(string(b), "exec.start") {
		t.Fatalf("the removal's jail.conf (%v):\n%s", err, b)
	}
	if err := h.RemoveJailRun(cid); err != nil {
		t.Error(err)
	}
}

func testUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
