package restore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"github.com/flyingEirc/Intern/internal/physical"
)

// prepareStep is one prepare invocation with its exact argv.
type prepareStep struct {
	opts physical.PrepareOptions
	args []string
}

// prepareSteps returns the exact prepare sequence for one chain.
//
// A chain with only a full backup needs one plain prepare. A chain with
// incrementals must apply every non-final link with --apply-log-only, and the
// final link without it; omitting it on an intermediate link is a premature
// rollback the acceptance suite fixes in place.
func prepareSteps(links []PlanLink, binary, workRoot string, useMemory int64) ([]prepareStep, error) {
	if len(links) == 0 {
		return nil, errors.New("restore: a prepare plan needs at least one link")
	}
	target := filepath.Join(workRoot, "0")
	if _, err := os.Lstat(target); err != nil {
		return nil, errors.New("restore: the extracted full backup is missing")
	}
	steps := make([]prepareStep, 0, len(links))
	add := func(applyLogOnly bool, incremental string) error {
		opts := physical.PrepareOptions{
			Binary: binary, TargetDir: target, ApplyLogOnly: applyLogOnly,
			IncrementalDir: incremental, UseMemory: useMemory,
		}
		args, err := physical.PrepareArgs(opts)
		if err != nil {
			return err
		}
		steps = append(steps, prepareStep{opts: opts, args: args})
		return nil
	}
	if len(links) == 1 {
		if err := add(false, ""); err != nil {
			return nil, err
		}
		return steps, nil
	}
	if err := add(true, ""); err != nil {
		return nil, err
	}
	for index := 1; index < len(links); index++ {
		incremental := filepath.Join(workRoot, strconv.Itoa(index))
		if _, err := os.Lstat(incremental); err != nil {
			return nil, errors.New("restore: an extracted incremental is missing")
		}
		// Every link except the last one keeps the redo log unapplied.
		if err := add(index < len(links)-1, incremental); err != nil {
			return nil, err
		}
	}
	return steps, nil
}

func (e *execution) prepareChain(ctx context.Context) error {
	steps, err := prepareSteps(e.plan.Links, e.opts.XtrabackupPath, e.work, e.opts.UseMemory)
	if err != nil {
		return stage(CodePrepareFailed, err)
	}
	for index, step := range steps {
		e.phase = PhasePreparing
		e.report(PhasePreparing, map[string]any{"step": index, "steps": len(steps), "argv": step.args})
		if err = physical.Prepare(ctx, step.opts); err != nil {
			return e.toolFailure(CodePrepareFailed, err)
		}
		e.prepareArgs = append(e.prepareArgs, step.args)
	}
	return e.verifyPrepared()
}

func (e *execution) verifyPrepared() error {
	// #nosec G304 -- the path is fixed inside this restore's private work directory.
	raw, err := os.ReadFile(filepath.Join(e.work, "0", "xtrabackup_checkpoints"))
	if err != nil {
		return stage(CodePrepareFailed, errors.New("restore: prepared checkpoints are missing"))
	}
	checkpoints, err := physical.ParseCheckpoints(raw)
	if err != nil {
		return stage(CodePrepareFailed, errors.New("restore: prepared checkpoints are invalid"))
	}
	if checkpoints.BackupType != "full-prepared" || checkpoints.ToLSN != e.plan.TargetToLSN {
		return stage(CodePrepareFailed, errors.New("restore: the prepared chain does not reach the target LSN"))
	}
	return nil
}

// renamePrepared moves the prepared full directory to the rehearsal name in the
// same private work directory.
func (e *execution) renamePrepared() error {
	source := filepath.Join(e.work, "0")
	target := filepath.Join(e.work, "data")
	if _, err := os.Lstat(target); err == nil {
		return errors.New("restore: a prepared data directory already exists")
	}
	// #nosec G301 -- both paths are fixed inside this restore's private work directory.
	if err := os.Rename(source, target); err != nil {
		return errors.New("restore: cannot rename the prepared directory")
	}
	return nil
}
