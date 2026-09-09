package api

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

type txStateWriter struct{ tx *sql.Tx }

func (w txStateWriter) UpdateInstanceState(
	ctx context.Context, id, from, to string,
) (bool, error) {
	return store.TxUpdateInstanceState(ctx, w.tx, id, from, to)
}

func setStateTx(
	ctx context.Context, tx *sql.Tx, id string, from, to instance.State,
) (bool, error) {
	return instance.SetState(ctx, txStateWriter{tx: tx}, id, from, to)
}

// holdStateTx atomically asserts a state while claiming a job that has no transition.
func holdStateTx(ctx context.Context, tx *sql.Tx, id string, state instance.State) (bool, error) {
	return store.TxUpdateInstanceState(ctx, tx, id, string(state), string(state))
}

func finishProvisioningState(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	from, to instance.State,
	containerID, gameBuildID string,
) error {
	if err := instance.ValidateTransition(from, to); err != nil {
		return err
	}
	return store.TxFinishProvisioning(
		ctx, tx, id, string(from), string(to), containerID, gameBuildID,
	)
}

func finishStartState(
	ctx context.Context, tx *sql.Tx, id string, from, to instance.State,
) error {
	if err := instance.ValidateTransition(from, to); err != nil {
		return fmt.Errorf("finish start: %w", err)
	}
	return store.TxFinishStart(ctx, tx, id, string(from), string(to))
}
