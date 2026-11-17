package containers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/greenbeanorg/skep/pkg/models"
)

func static(addr, gw string) *models.ContainerIPv4 {
	return &models.ContainerIPv4{Mode: models.IPv4Static, Address: addr, Gateway: gw}
}

func TestNetworkStatic(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "web1", TemplateID: f.withSSHD, SSHAuthorizedKey: sshKey(),
		NICs: []models.ContainerNICSpec{
			{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "192.0.2.1")},
			{Bridge: "bridge1", MACAddress: "02:AA:00:00:00:02", IPv4: static("198.51.100.5/25", "")},
		},
		Nameservers: []string{"192.0.2.53", "2001:db8::53"}})
	if len(c.NICs) != 2 {
		t.Fatalf("NICs = %+v", c.NICs)
	}
	// Fixed guest names by slot, whatever the container ID (amendment k).
	jif0, jif1 := "skep0", "skep1"
	n0, n1 := c.NICs[0], c.NICs[1]
	if n0.Slot != 0 || n0.Interface != jif0 || !strings.HasPrefix(n0.MACAddress, "02:") || n0.IPv4 != *static("192.0.2.10/24", "192.0.2.1") ||
		n1.Interface != jif1 || n1.MACAddress != "02:aa:00:00:00:02" || n1.Bridge != "bridge1" || n0.HostInterface != "" {
		t.Fatalf("NICs = %+v", c.NICs)
	}
	files, ok := f.fake.JailFiles(c.ID)
	want := []string{
		`ifconfig_` + jif0 + `="inet 192.0.2.10/24"`, `defaultrouter="192.0.2.1"`,
		`ifconfig_` + jif1 + `="inet 198.51.100.5/25"`,
		`sshd_enable="YES"`, `sshd_flags="-o PermitRootLogin=prohibit-password"`,
	}
	if !ok || !slices.Equal(files.RCConf, want) || !slices.Equal(files.Nameservers, []string{"192.0.2.53", "2001:db8::53"}) {
		t.Fatalf("files = %+v", files)
	}

	// Start: one epair per NIC, recorded by slot; a static jail does not
	// touch devfs.
	done := f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	var res ActionResult
	_ = json.Unmarshal(done.Result, &res)
	if len(res.Epairs) != 2 {
		t.Fatalf("start result = %s", done.Result)
	}
	c, _ = f.svc.Container(ctx, c.ID)
	if c.NICs[0].HostInterface != res.Epairs[0] || c.NICs[1].HostInterface != res.Epairs[1] || len(f.fake.Epairs()) != 2 {
		t.Fatalf("running NICs = %+v, epairs %v", c.NICs, f.fake.Epairs())
	}
	calls := strings.Join(f.fake.Calls(), "\n")
	if strings.Contains(calls, "devfs") {
		t.Fatal("a static jail touched devfs")
	}

	// Stop: the epairs are gone and forgotten.
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))
	c, _ = f.svc.Container(ctx, c.ID)
	if c.NICs[0].HostInterface != "" || len(f.fake.Epairs()) != 0 {
		t.Fatalf("after stop: NICs %+v, epairs %v", c.NICs, f.fake.Epairs())
	}

	// After the first start: the addressing and nameservers are the
	// jail's own; a MAC or bridge change is allowed.
	moved := []models.ContainerNICSpec{
		{Bridge: "bridge1", IPv4: static("192.0.2.10/24", "192.0.2.1")},
		{Bridge: "bridge1", MACAddress: "02:aa:00:00:00:03", IPv4: static("198.51.100.5/25", "")},
	}
	f.succeed(f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{NICs: &moved}, f.sub))
	c, _ = f.svc.Container(ctx, c.ID)
	if c.NICs[0].Bridge != "bridge1" || c.NICs[0].MACAddress != n0.MACAddress || c.NICs[0].ID != n0.ID || c.NICs[1].MACAddress != "02:aa:00:00:00:03" {
		t.Fatalf("after the bridge and MAC change: %+v", c.NICs)
	}
	readdressed := []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.11/24", "192.0.2.1")}}
	_, err := f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{NICs: &readdressed}, f.sub)
	wantCode(t, err, models.CodeContainerStarted)
	ns := []string{"9.9.9.9"}
	_, err = f.svc.UpdateContainer(ctx, c.ID, models.ContainerUpdate{Nameservers: &ns}, f.sub)
	wantCode(t, err, models.CodeContainerStarted)

	// Delete removes an epair a crash left, and says so.
	left := f.fake.LeaveEpair(c.ID, 0)
	done = f.succeed(f.svc.DeleteContainer(ctx, c.ID, f.sub))
	if !strings.Contains(string(done.Result), `"interfaces_destroyed":["`+left+`"]`) || len(f.fake.Epairs()) != 0 {
		t.Fatalf("delete result = %s, epairs %v", done.Result, f.fake.Epairs())
	}
}

func TestNetworkDHCP(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "d1", TemplateID: f.minimal, NICs: []models.ContainerNICSpec{{Bridge: "bridge0"}}})
	jif := "skep0"
	files, ok := f.fake.JailFiles(c.ID)
	if !ok || !slices.Equal(files.RCConf, []string{`ifconfig_` + jif + `="SYNCDHCP"`}) || len(files.Nameservers) != 0 || files.AuthorizedKey != "" {
		t.Fatalf("files = %+v", files)
	}
	if c.NICs[0].IPv4.Mode != models.IPv4DHCP {
		t.Fatalf("NIC = %+v", c.NICs[0])
	}
	// An epair a crash left is removed by the next start.
	left := f.fake.LeaveEpair(c.ID, 0)
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub))
	if _, ok := f.fake.Epairs()[left]; ok {
		t.Fatalf("the leftover %s survived the start", left)
	}
	calls := strings.Join(f.fake.Calls(), "\n")
	if !strings.Contains(calls, "devfs rule -s 6001 add") || !strings.Contains(calls, "ifconfig "+left+" destroy") {
		t.Fatalf("calls:\n%s", calls)
	}
	f.succeed(f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "stop"}, f.sub))

	// Before the first start the addressing could change and the files
	// were rewritten; here it has started, so check a fresh container.
	c2 := f.create(models.ContainerCreate{Name: "d2", TemplateID: f.minimal, NICs: []models.ContainerNICSpec{{Bridge: "bridge0"}}})
	readdressed := []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.20/24", "192.0.2.1")}}
	ns := []string{"192.0.2.53"}
	f.succeed(f.svc.UpdateContainer(ctx, c2.ID, models.ContainerUpdate{NICs: &readdressed, Nameservers: &ns}, f.sub))
	jif2 := "skep0"
	files, _ = f.fake.JailFiles(c2.ID)
	if !slices.Equal(files.RCConf, []string{`ifconfig_` + jif2 + `="inet 192.0.2.20/24"`, `defaultrouter="192.0.2.1"`}) ||
		!slices.Equal(files.Nameservers, ns) {
		t.Fatalf("files after the update = %+v", files)
	}
	c2, _ = f.svc.Container(ctx, c2.ID)
	if c2.NICs[0].MACAddress != c.NICs[0].MACAddress && c2.NICs[0].MACAddress == "" {
		t.Fatalf("NIC = %+v", c2.NICs[0])
	}
}

func TestNetworkRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	many := make([]models.ContainerNICSpec, models.MaxContainerNICs+1)
	for i := range many {
		many[i] = models.ContainerNICSpec{Bridge: "bridge0"}
	}
	cases := []struct {
		name string
		req  models.ContainerCreate
		code string
	}{
		{"not allow-listed", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge9"}}}, models.CodeBridgeNotAllowed},
		{"bad bridge name", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0;x"}}}, models.CodeInvalidRequest},
		{"too many", models.ContainerCreate{NICs: many}, models.CodeInvalidRequest},
		{"nameservers with dhcp", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0"}}, Nameservers: []string{"9.9.9.9"}}, models.CodeInvalidRequest},
		{"two gateways", models.ContainerCreate{NICs: []models.ContainerNICSpec{
			{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "192.0.2.1")}, {Bridge: "bridge0", IPv4: static("198.51.100.5/24", "198.51.100.1")}}}, models.CodeInvalidRequest},
		{"gateway outside", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "192.0.3.1")}}}, models.CodeInvalidRequest},
		{"gateway is the address", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "192.0.2.10")}}}, models.CodeInvalidRequest},
		{"network address", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.0/24", "")}}}, models.CodeInvalidRequest},
		{"broadcast", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.255/24", "")}}}, models.CodeInvalidRequest},
		{"no prefix", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10", "")}}}, models.CodeInvalidRequest},
		{"IPv6 address", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("2001:db8::1/64", "")}}}, models.CodeInvalidRequest},
		{"loopback", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("127.0.0.2/8", "")}}}, models.CodeInvalidRequest},
		{"bad mode", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: &models.ContainerIPv4{Mode: "slaac"}}}}, models.CodeInvalidRequest},
		{"address with dhcp", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: &models.ContainerIPv4{Mode: "dhcp", Address: "192.0.2.10/24"}}}}, models.CodeInvalidRequest},
		{"multicast MAC", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", MACAddress: "01:00:5e:00:00:01"}}}, models.CodeInvalidRequest},
		{"same MAC twice", models.ContainerCreate{NICs: []models.ContainerNICSpec{
			{Bridge: "bridge0", MACAddress: "02:00:00:00:00:09"}, {Bridge: "bridge0", MACAddress: "02:00:00:00:00:09"}}}, models.CodeInvalidRequest},
		{"bad nameserver", models.ContainerCreate{NICs: []models.ContainerNICSpec{{Bridge: "bridge0", IPv4: static("192.0.2.10/24", "")}}, Nameservers: []string{"ns1"}}, models.CodeInvalidRequest},
	}
	for _, tc := range cases {
		tc.req.Name, tc.req.TemplateID = "x", f.minimal
		_, err := f.svc.CreateContainer(ctx, tc.req, f.sub)
		var me *models.Error
		if !asError(err, &me) || me.Code != tc.code {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.code)
		}
	}
	// A MAC another jail has.
	f.create(models.ContainerCreate{Name: "a", TemplateID: f.minimal, NICs: []models.ContainerNICSpec{{Bridge: "bridge0", MACAddress: "02:00:00:00:00:0a"}}})
	created, err := f.svc.CreateContainer(ctx, models.ContainerCreate{Name: "b", TemplateID: f.minimal,
		NICs: []models.ContainerNICSpec{{Bridge: "bridge0", MACAddress: "02:00:00:00:00:0a"}}}, f.sub)
	if err == nil {
		t.Fatalf("a duplicate MAC was accepted: %+v", created)
	}
}

func TestNetworkStartRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.create(models.ContainerCreate{Name: "n1", TemplateID: f.minimal, NICs: []models.ContainerNICSpec{{Bridge: "bridge0"}}})
	plain := f.create(models.ContainerCreate{Name: "n2", TemplateID: f.minimal})

	// Without if_epair: refused before a task exists, audited; a jail
	// without a NIC still starts.
	f.fake.NoEpair = true
	_, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub)
	wantCode(t, err, models.CodeEpairUnavailable)
	if n := f.count(`SELECT count(*) FROM audit_log WHERE outcome = 'denied' AND resource_id = ? AND details LIKE '%' || ? || '%'`, c.ID, models.CodeEpairUnavailable); n != 1 {
		t.Fatalf("%d denials audited", n)
	}
	f.succeed(f.svc.Action(ctx, plain.ID, models.ContainerAction{Action: "start"}, f.sub))
	f.fake.NoEpair = false

	// A foreign devfs ruleset 6001: the start fails with the code and
	// leaves no jail and no epair.
	f.fake.DevfsConflict = true
	task, err := f.svc.Action(ctx, c.ID, models.ContainerAction{Action: "start"}, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	done := f.wait(task.ID)
	if done.Status != models.TaskFailed || done.ErrorCode != models.CodeDevfsRulesetConflict {
		t.Fatalf("start = %s %s %s", done.Status, done.ErrorCode, done.ErrorMessage)
	}
	if running, _ := f.svc.isRunning(ctx, c.ID); running || len(f.fake.Epairs()) != 0 {
		t.Fatalf("running %v, epairs %v", running, f.fake.Epairs())
	}
}

func TestIdempotentCreateWithGeneratedMAC(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.sub.IdempotencyKey = "k-nic"
	req := models.ContainerCreate{Name: "i1", TemplateID: f.minimal, NICs: []models.ContainerNICSpec{{Bridge: "bridge0"}}}
	first, err := f.svc.CreateContainer(ctx, req, f.sub)
	if err != nil {
		t.Fatal(err)
	}
	f.succeed(first.Task, nil)
	again, err := f.svc.CreateContainer(ctx, req, f.sub)
	if err != nil || again.Task.ID != first.Task.ID || again.Container.ID != first.Container.ID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
}

func asError(err error, target **models.Error) bool { return errors.As(err, target) }
