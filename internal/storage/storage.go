// Package storage is the agent's storage service: the inventory of what
// Skep owns in each allow-listed pool (ADR 0010 decision 1). It reads the
// host through internal/hostops and the agent's database, knows nothing
// about HTTP, and changes nothing.
package storage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/greenbeanorg/skep/internal/database"
	"github.com/greenbeanorg/skep/internal/hostops"
	"github.com/greenbeanorg/skep/pkg/models"
)

// Page sizes of the inventory.
const (
	DefaultPageSize = 100
	MaxPageSize     = 500
)

// OwnerStore says which resources exist.
type OwnerStore interface {
	Owners(ctx context.Context) (database.Owners, error)
}

// Options configures a Service.
type Options struct {
	Host   hostops.Ops
	Owners OwnerStore
	Logger *slog.Logger
}

// Service implements the storage operations of the internal API.
type Service struct {
	host   hostops.Ops
	owners OwnerStore
	log    *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.Host.ZFS == nil || opts.Owners == nil {
		return nil, errors.New("storage: hostops and an owner store are required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{host: opts.Host, owners: opts.Owners, log: opts.Logger}, nil
}

// Datasets returns a page of the inventory, ordered by pool (in the order
// of the allow-list) and then by name. The cursor is the full name of the
// last dataset of the previous page.
func (s *Service) Datasets(ctx context.Context, opts models.DatasetListOptions) (models.DatasetList, error) {
	switch {
	case opts.Limit == 0:
		opts.Limit = DefaultPageSize
	case opts.Limit < 0 || opts.Limit > MaxPageSize:
		return models.DatasetList{}, models.Invalid(fmt.Sprintf("limit must be between 1 and %d", MaxPageSize))
	}
	pools := s.host.Conf.Pools
	if opts.Pool != "" {
		if s.host.Conf.IsDirectory(opts.Pool) {
			return models.DatasetList{}, &models.Error{Code: models.CodeStorageFeatureUnsupported,
				Message: "the inventory lists ZFS datasets; " + opts.Pool + " is a directory pool, whose disk files are listed with their VMs",
				Details: map[string]any{"pool": opts.Pool, "pool_type": hostops.PoolKindDirectory}}
		}
		if err := s.host.Conf.CheckPool(opts.Pool); err != nil {
			return models.DatasetList{}, err
		}
		pools = []string{opts.Pool}
	}
	afterPool, afterName := 0, ""
	if opts.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(opts.Cursor)
		pool, _, _ := strings.Cut(string(raw), "/")
		idx := indexOf(pools, pool)
		if err != nil || idx < 0 {
			return models.DatasetList{}, models.Invalid("invalid cursor")
		}
		afterPool, afterName = idx, string(raw)
	}
	owners, err := s.owners.Owners(ctx)
	if err != nil {
		return models.DatasetList{}, err
	}

	list := models.DatasetList{Items: []models.Dataset{}}
	for i := afterPool; i < len(pools); i++ {
		pool := pools[i]
		found, err := s.host.ZFS.Inventory(ctx, pool)
		if err != nil {
			var me *models.Error
			if ctx.Err() != nil || opts.Pool != "" || (errors.As(err, &me) && me.Code != models.CodeStorageUnavailable) {
				return models.DatasetList{}, err
			}
			// One pool that cannot be read does not hide the others.
			s.log.Warn("storage: cannot list a pool", "pool", pool, "error", err)
			list.UnavailablePools = append(list.UnavailablePools, pool)
			continue
		}
		root, _ := s.host.Conf.RootDataset(pool)
		byName := make(map[string]hostops.Dataset, len(found))
		for _, d := range found {
			if !d.IsSnapshot() {
				byName[d.Name] = d
			}
		}
		for _, d := range found {
			if i == afterPool && afterName != "" && d.Name <= afterName {
				continue
			}
			if len(list.Items) == opts.Limit {
				list.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(list.Items[len(list.Items)-1].Name))
				return list, nil
			}
			list.Items = append(list.Items, describe(pool, root, d, byName, owners))
		}
	}
	return list, nil
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func describe(pool, root string, d hostops.Dataset, datasets map[string]hostops.Dataset, owners database.Owners) models.Dataset {
	out := models.Dataset{
		Name: d.Name, Pool: pool, Type: d.Type, Owner: ClassifySnapshot(root, d, datasets, owners),
		UsedBytes: d.UsedBytes, ReferencedBytes: d.ReferencedBytes, Origin: d.Origin, Clones: d.Clones,
		Encryption: d.Encryption, KeyStatus: d.KeyStatus, Compression: d.Compression, CompressRatio: d.CompressRatio,
	}
	if d.Type == "volume" {
		out.VolSizeBytes, out.RefReservationBytes, out.VolBlockSize = d.VolSizeBytes, d.RefReservationBytes, d.VolBlockSize
	}
	if d.IsSnapshot() {
		_, short, _ := strings.Cut(d.Name, "@")
		if d.SnapshotID != "" && short == hostops.SnapshotPrefix+d.SnapshotID {
			out.SnapshotID = d.SnapshotID
		}
	}
	return out
}

// ClassifySnapshot is Classify, except that a snapshot is judged by the
// dataset it belongs to (whose owner property is the one that counts) and
// is unowned if Skep did not take it.
func ClassifySnapshot(root string, d hostops.Dataset, datasets map[string]hostops.Dataset, owners database.Owners) models.DatasetOwner {
	base, short, isSnap := strings.Cut(d.Name, "@")
	if !isSnap {
		return Classify(root, d, owners)
	}
	parent, ok := datasets[base]
	if !ok {
		return unowned("its dataset was not listed")
	}
	owner := Classify(root, parent, owners)
	if owner.Kind == models.DatasetOwnerUnowned {
		return owner
	}
	if d.SnapshotID == "" || short != hostops.SnapshotPrefix+d.SnapshotID {
		return unowned("a snapshot Skep did not take")
	}
	return owner
}

// places maps the dataset under <root> that holds one dataset per
// resource to the owner kind it holds.
var places = map[string]string{
	"vm":        models.DatasetOwnerVM,
	"jail":      models.DatasetOwnerContainer,
	"templates": models.DatasetOwnerTemplate,
}

func unowned(reason string) models.DatasetOwner {
	return models.DatasetOwner{Kind: models.DatasetOwnerUnowned, Reason: reason}
}

// Classify says what a dataset under root belongs to. A dataset is owned
// only when three things agree: its place in the tree, its skep: owner
// property, and a row for that owner in the database. A snapshot belongs
// to the owner of its dataset.
func Classify(root string, d hostops.Dataset, owners database.Owners) models.DatasetOwner {
	base, _, isSnap := strings.Cut(d.Name, "@")
	if base == root {
		if isSnap {
			return unowned("a snapshot Skep did not take")
		}
		return models.DatasetOwner{Kind: models.DatasetOwnerSkep}
	}
	rel, ok := strings.CutPrefix(base, root+"/")
	if !ok {
		return unowned("outside the dataset root")
	}
	parts := strings.Split(rel, "/")
	kind, known := places[parts[0]]
	if !known {
		return unowned("not a place where Skep creates datasets")
	}
	if len(parts) == 1 {
		if isSnap {
			return unowned("a snapshot Skep did not take")
		}
		return models.DatasetOwner{Kind: models.DatasetOwnerSkep}
	}
	id := parts[1]
	var prop string
	var exists bool
	switch kind {
	case models.DatasetOwnerVM:
		prop, exists = d.VMID, owners.VMs[id]
		// A VM's tree is its filesystem and one zvol per disk.
		if len(parts) > 3 || (len(parts) == 3 && !isDisk(parts[2])) {
			return unowned("not a place where Skep creates datasets")
		}
	case models.DatasetOwnerContainer:
		prop, exists = d.ContainerID, owners.Containers[id]
	case models.DatasetOwnerTemplate:
		prop, exists = d.TemplateID, owners.Templates[id]
	}
	switch {
	case !hostops.ValidID(id):
		return unowned("the name is not a resource ID")
	case prop == "":
		return unowned("it has no skep: owner property")
	case prop != id:
		return unowned("its skep: owner property does not match its name")
	case !exists:
		return unowned("its owner does not exist in the agent's database")
	}
	return models.DatasetOwner{Kind: kind, ID: id}
}

func isDisk(s string) bool {
	n, ok := strings.CutPrefix(s, "disk")
	if !ok || n == "" || len(n) > 2 {
		return false
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
