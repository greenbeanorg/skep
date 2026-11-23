package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AdminChecker answers whether a user exists in the API's database. The
// plan asks it only when api.db exists.
type AdminChecker interface {
	// AdminExists reports whether username exists, with skep-api's own
	// words for the plan; err when the question could not be answered.
	AdminExists(ctx context.Context, root, username string) (found bool, detail string, err error)
}

// SkepAPIAdmins asks "skep-api admin exists" (exit 0: exists, 1: does
// not, anything else: error), run as the skep user with the fixed
// environment, never as root. The command opens api.db read-only and never
// creates or migrates it, so check and plan stay read-only.
type SkepAPIAdmins struct {
	Runner Runner
}

// AdminExists implements AdminChecker.
func (s SkepAPIAdmins) AdminExists(ctx context.Context, root, username string) (bool, string, error) {
	cred, err := skepCredential(root)
	if err != nil {
		return false, "", err
	}
	svc, err := apiServiceArgs(root)
	if err != nil {
		return false, "", err
	}
	out, err := s.Runner.Run(ctx, Cmd{Name: hostPath(root, apiBin),
		Args: append([]string{"admin", "exists", "--username", username}, svc...), As: cred})
	detail := strings.TrimSpace(out)
	if err == nil {
		return true, detail, nil
	}
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) && coded.ExitCode() == 1 {
		return false, detail, nil
	}
	return false, "", err
}

// NoAdminChecker is the checker where skep-api cannot be asked; every
// question fails with Reason.
type NoAdminChecker struct{ Reason string }

// AdminExists implements AdminChecker.
func (n NoAdminChecker) AdminExists(context.Context, string, string) (bool, string, error) {
	return false, "", fmt.Errorf("%s", n.Reason)
}
