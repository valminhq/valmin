// Package rotation runs the job that moves every stored secret to the active key generation.
package rotation

import (
	"context"
	"fmt"

	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/errcode"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
)

// Job returns the runner of a key-rotation job over db's encrypted columns.
func Job(db *store.DB, keeper *crypto.Keeper) jobs.Runner {
	r := &rotator{db: db, keeper: keeper}
	return r.run
}

type rotator struct {
	db     *store.DB
	keeper *crypto.Keeper
}

const (
	masterKeyCaveat = "The master key is unchanged, so this does not remediate a leaked secret.key."
	keyRotateBatch  = 100
)

// run publishes a generation and rewrites every encrypted column under it.
//
// The generation is published before the first row is swept, which is what keeps a
// concurrent secret write out of the sweep's way: from the flip onwards every writer seals
// under the new generation, so a row the sweep has already passed cannot come back sealed
// under an old one. The sweep ends on a pass that finds nothing outstanding, which is that
// verification rather than a separate one.
func (r *rotator) run(ctx context.Context, jh *jobs.Handle) jobs.Outcome {
	fail := func(err error) jobs.Outcome {
		return jobs.Outcome{
			Status: jobs.StatusFailed, ErrorCode: errcode.Internal.String(), Error: err.Error(),
		}
	}

	generation, outstanding, err := r.generation(ctx)
	if err != nil {
		return fail(err)
	}
	jh.Progress(ctx, 0, fmt.Sprintf("Rotating derived keys to generation %s", generation))

	swept := 0
	for {
		if ctx.Err() != nil {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		batch, err := r.db.ListStaleSecrets(ctx, generation, keyRotateBatch)
		if err != nil {
			return fail(fmt.Errorf("list outstanding secrets: %w", err))
		}
		if len(batch) == 0 {
			break
		}
		moved, err := r.sweep(ctx, generation, batch)
		swept += moved
		if err != nil {
			return fail(err)
		}
		if ctx.Err() != nil {
			return jobs.Outcome{Status: jobs.StatusCancelled}
		}
		if moved == 0 {
			// Every row in the batch was left behind, so the next read returns the same
			// rows: stop rather than spin.
			return fail(fmt.Errorf("%d secrets are still on an older generation than %s",
				len(batch), generation))
		}
		if outstanding > 0 {
			jh.Progress(ctx, min(100*swept/outstanding, 99),
				fmt.Sprintf("Re-encrypted %d of %d secrets", swept, outstanding))
		}
	}

	jh.Progress(ctx, 100, fmt.Sprintf(
		"Derived keys are on generation %s; %d secrets re-encrypted. %s",
		generation, swept, masterKeyCaveat))
	return jobs.Outcome{Status: jobs.StatusSucceeded}
}

// generation settles which generation this run sweeps onto, and how much it owes.
//
// A rotation mints one only when nothing is outstanding: a retry after an interrupted sweep
// finishes the rows still on an older generation under the generation already published,
// rather than adding a third for the same rotation. The envelopes are the progress record
// , so there is none to keep in step.
func (r *rotator) generation(ctx context.Context) (id string, outstanding int, err error) {
	id = r.keeper.ActiveKeyID()
	if outstanding, err = r.db.CountStaleSecrets(ctx, id); err != nil {
		return "", 0, fmt.Errorf("count outstanding secrets: %w", err)
	}
	if outstanding > 0 {
		return id, outstanding, nil
	}
	if id, err = r.keeper.Rotate(ctx, r.db); err != nil {
		return "", 0, fmt.Errorf("publish the next key generation: %w", err)
	}
	if outstanding, err = r.db.CountStaleSecrets(ctx, id); err != nil {
		return "", 0, fmt.Errorf("count outstanding secrets: %w", err)
	}
	return id, outstanding, nil
}

// sweep re-seals one batch and returns how many rows moved. A row whose conditional update
// matches nothing was rewritten by a secret writer while the sweep held it, and that writer
// sealed under the active generation, so it is done rather than retried.
//
// A batch runs to its end: shutdown is observed at the batch boundary, since every row here
// is one crypto operation and one indexed update and every committed row is work the next
// run does not repeat.
func (r *rotator) sweep(ctx context.Context, generation string, batch []store.StaleSecret) (int, error) {
	moved := 0
	for i := range batch {
		s := &batch[i]
		purpose := crypto.Purpose(s.Purpose)
		loc := crypto.Location{Table: s.Table, Column: s.Column, RowID: s.RowID}
		plaintext, err := r.keeper.Decrypt(purpose, loc, s.Envelope)
		if err != nil {
			return moved, fmt.Errorf("read %s.%s of row %s: %w", s.Table, s.Column, s.RowID, err)
		}
		sealed, err := r.keeper.Encrypt(purpose, loc, plaintext)
		if err != nil {
			return moved, fmt.Errorf("seal %s.%s of row %s under generation %s: %w",
				s.Table, s.Column, s.RowID, generation, err)
		}
		replaced, err := r.db.ReplaceSecret(ctx, s, sealed)
		if err != nil {
			return moved, fmt.Errorf("rotate %s.%s of row %s: %w", s.Table, s.Column, s.RowID, err)
		}
		if replaced {
			moved++
		}
	}
	return moved, nil
}
