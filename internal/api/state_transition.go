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
	ok, err := store.TxUpdateInstanceState(ctx, w.tx, id, from, to)
	if err != nil {
		return false, fmt.Errorf("write state transition: %w", err)
	}
	return ok, nil
}

func setStateTx(
	ctx context.Context, tx *sql.Tx, id string, from, to instance.State,
) (bool, error) {
	ok, err := instance.SetState(ctx, txStateWriter{tx: tx}, id, from, to)
	if err != nil {
		return false, fmt.Errorf("transition instance state: %w", err)
	}
	return ok, nil
}

// holdStateTx atomically asserts a state while claiming a job that has no transition.
func holdStateTx(ctx context.Context, tx *sql.Tx, id string, state instance.State) (bool, error) {
	ok, err := store.TxUpdateInstanceState(ctx, tx, id, string(state), string(state))
	if err != nil {
		return false, fmt.Errorf("assert instance %s state %s: %w", id, state, err)
	}
	return ok, nil
}

func finishProvisioningState(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	from, to instance.State,
	containerID, gameBuildID string,
) error {
	if err := instance.ValidateTransition(from, to); err != nil {
		return fmt.Errorf("finish provisioning: %w", err)
	}
	if err := store.TxFinishProvisioning(
		ctx, tx, id, string(from), string(to), containerID, gameBuildID,
	); err != nil {
		return fmt.Errorf("finish provisioning state: %w", err)
	}
	return nil
}

func finishStartState(
	ctx context.Context, tx *sql.Tx, id string, from, to instance.State,
) error {
	if err := instance.ValidateTransition(from, to); err != nil {
		return fmt.Errorf("finish start: %w", err)
	}
	if err := store.TxFinishStart(ctx, tx, id, string(from), string(to)); err != nil {
		return fmt.Errorf("finish start state: %w", err)
	}
	return nil
}
