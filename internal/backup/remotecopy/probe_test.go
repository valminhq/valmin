package remotecopy

import (
	"context"
	"testing"

	"github.com/valminhq/valmin/internal/backup/remote"
)

// slowBackend stores what Put sends and then reports the attempt's deadline as passed, the way
// a server that answers a stored upload too late looks to the panel.
type slowBackend struct {
	objects map[string]bool
}

func (b *slowBackend) Check(context.Context) error { return nil }

func (b *slowBackend) Put(ctx context.Context, key, _ string) (remote.Object, error) {
	b.objects[key] = true
	<-ctx.Done()
	return remote.Object{}, &remote.Failure{Message: "remote request failed", Temporary: true}
}

func (b *slowBackend) Stat(_ context.Context, ref remote.ObjectRef) (remote.Object, error) {
	return remote.Object{Ref: ref}, nil
}

func (b *slowBackend) Delete(ctx context.Context, ref remote.ObjectRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delete(b.objects, ref.Key)
	return nil
}

// TestAProbeThatTimesOutRemovesItsObject asserts a probe whose upload outlives its deadline
// still deletes the probe object it may have left on the remote.
func TestAProbeThatTimesOutRemovesItsObject(t *testing.T) {
	b := &slowBackend{objects: map[string]bool{}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := probe(ctx, b, "dest-1"); err == nil {
		t.Fatal("probe succeeded; want the upload's failure")
	}
	if len(b.objects) != 0 {
		t.Errorf("remote still holds %v after the probe", b.objects)
	}
}
