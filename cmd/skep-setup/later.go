package main

import (
	"context"
	"errors"
)

var errNotYet = errors.New("not built yet")

func runUpgrade(context.Context, []string, env) error { return errNotYet }
func runRemove(context.Context, []string, env) error  { return errNotYet }
