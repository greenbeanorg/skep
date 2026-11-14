package internalapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/greenbeanorg/skep/internal/agentproto"
	"github.com/greenbeanorg/skep/internal/internalapi"
	"github.com/greenbeanorg/skep/pkg/models"
)

type fakeStorage struct{ got models.DatasetListOptions }

func (f *fakeStorage) Datasets(_ context.Context, opts models.DatasetListOptions) (models.DatasetList, error) {
	f.got = opts
	if opts.Pool == "zroot" {
		return models.DatasetList{}, &models.Error{Code: models.CodePoolNotAllowed, Message: "no"}
	}
	return models.DatasetList{Items: []models.Dataset{{Name: "skeptest/skep", Pool: "skeptest", Type: "filesystem",
		Owner: models.DatasetOwner{Kind: models.DatasetOwnerSkep}}}, NextCursor: "next"}, nil
}

func TestStorageDatasetsThroughTheSocket(t *testing.T) {
	st := &fakeStorage{}
	path := startWith(t, &fakeBackend{}, me(), internalapi.Options{Storage: st})
	c := newClient(t, path)

	list, err := c.StorageDatasets(callerCtx(), models.DatasetListOptions{Pool: "skeptest", Limit: 5, Cursor: "abc"})
	if err != nil || len(list.Items) != 1 || list.NextCursor != "next" || list.Items[0].Owner.Kind != "skep" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if st.got != (models.DatasetListOptions{Pool: "skeptest", Limit: 5, Cursor: "abc"}) {
		t.Errorf("options = %+v", st.got)
	}
	_, err = c.StorageDatasets(callerCtx(), models.DatasetListOptions{Pool: "zroot"})
	var me *models.Error
	if !errors.As(err, &me) || me.Code != models.CodePoolNotAllowed {
		t.Errorf("err = %v", err)
	}
	// Read-only.
	hdr := http.Header{agentproto.HeaderActorID: {"user-1"}, agentproto.HeaderActorName: {"andy"}}
	if resp, _ := raw(t, path, http.MethodPost, agentproto.Prefix+"/storage/datasets", hdr); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", resp.StatusCode)
	}
	if resp, _ := raw(t, path, http.MethodGet, agentproto.Prefix+"/storage/datasets?limit=0", hdr); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("limit=0: %d", resp.StatusCode)
	}
}
