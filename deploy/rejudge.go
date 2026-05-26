package deploy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mrhaoxx/SOJ/file_transfer"
	"github.com/mrhaoxx/SOJ/judge"
	"github.com/mrhaoxx/SOJ/types"
)

// RejudgeOptions controls Rejudge behavior.
type RejudgeOptions struct {
	ProblemId string // empty = all problems
	AutoYes   bool   // skip the confirmation prompt
	Input     io.Reader
	Output    io.Writer
}

// Rejudge re-runs the evaluator on the latest submission per (user, problem)
// matching the filter. After all runs, BestScore for each touched (user, problem)
// is overwritten with the freshly judged value × current weight (not max-merged),
// so weight or judge.sh changes take full effect.
func Rejudge(cfg *types.Config, opts RejudgeOptions) error {
	input := opts.Input
	if input == nil {
		input = os.Stdin
	}
	output := opts.Output
	if output == nil {
		output = os.Stdout
	}

	dbService, err := types.NewDatabaseService(cfg)
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}

	problemManager := judge.NewProblemManager()
	problems := problemManager.LoadProblemDir(cfg.ProblemsDir)

	if opts.ProblemId != "" {
		if _, ok := problems[opts.ProblemId]; !ok {
			return fmt.Errorf("unknown problem %q", opts.ProblemId)
		}
	}

	// Pull every previously-evaluated submit (completed or failed; skip "dead"
	// which never finished). Newest first so the first occurrence of each
	// (user, problem) is the latest.
	db := dbService.GetDB()
	var allSubmits []types.SubmitCtx
	q := db.Where("status IN ?", []string{"completed", "failed"}).Order("submit_time desc")
	if opts.ProblemId != "" {
		q = q.Where("problem = ?", opts.ProblemId)
	}
	if err := q.Find(&allSubmits).Error; err != nil {
		return fmt.Errorf("query submits: %w", err)
	}

	type key struct{ user, problem string }
	seen := map[key]bool{}
	var targets []types.SubmitCtx
	for _, s := range allSubmits {
		k := key{s.User, s.Problem}
		if seen[k] {
			continue
		}
		if _, ok := problems[s.Problem]; !ok {
			continue // problem definition deleted; skip
		}
		seen[k] = true
		targets = append(targets, s)
	}

	if len(targets) == 0 {
		fmt.Fprintln(output, "No submissions to rejudge.")
		return nil
	}

	sort.Slice(targets, func(i, j int) bool {
		if targets[i].User != targets[j].User {
			return targets[i].User < targets[j].User
		}
		return targets[i].Problem < targets[j].Problem
	})

	scope := "all problems"
	if opts.ProblemId != "" {
		scope = "problem " + opts.ProblemId
	}
	fmt.Fprintf(output, "About to rejudge %d submission(s) for %s:\n", len(targets), scope)
	for _, s := range targets {
		fmt.Fprintf(output, "  - %s / %s   (latest submit %s, %s)\n",
			s.User, s.Problem, s.ID, time.Unix(0, s.SubmitTime).Format(time.DateTime))
	}

	if !opts.AutoYes {
		fmt.Fprint(output, "Continue? [y/N] ")
		reader := bufio.NewReader(input)
		line, _ := reader.ReadString('\n')
		ans := strings.TrimSpace(strings.ToLower(line))
		if ans != "y" && ans != "yes" {
			fmt.Fprintln(output, "Aborted.")
			return nil
		}
	}

	sandboxService := file_transfer.NewApptainerService()
	evaluator := judge.NewEvaluator(cfg, sandboxService, dbService)

	var okCnt, failCnt int
	for _, old := range targets {
		pb := problems[old.Problem]

		fmt.Fprintf(output, "\n=== Rejudging %s / %s ===\n", old.User, old.Problem)

		// Invalidate every prior submit for this (user, problem) BEFORE the
		// new evaluation — historical rows stay in the DB for audit but no
		// longer participate in rank calculations. The new submit below is
		// born with Invalid=false (zero value), so it becomes the only
		// valid one and RecomputeUserProblemBest picks it up.
		if n, err := dbService.MarkSubmitsInvalidByUserProblem(old.User, old.Problem); err != nil {
			fmt.Fprintf(output, "  ! failed to invalidate prior submits for %s/%s: %v\n", old.User, old.Problem, err)
			failCnt++
			continue
		} else if n > 0 {
			fmt.Fprintf(output, "  invalidated %d prior submission(s)\n", n)
		}

		subtime := time.Now()
		id := strconv.Itoa(int(subtime.UnixNano()))
		ctx := types.SubmitCtx{
			ID:         id,
			Problem:    old.Problem,
			User:       old.User,
			SubmitTime: subtime.UnixNano(),
			Status:     "init",
			SubmitDir:  path.Join(cfg.SubmitsDir, old.User, old.Problem),
			Workdir:    path.Join(cfg.SubmitWorkDir, id),

			RealWorkdir: path.Join(cfg.RealSubmitWorkDir, id),

			Userface: types.Userface{
				Buffer: bytes.NewBuffer(nil),
				Writer: output,
			},
			Running: make(chan struct{}),
		}

		fmt.Fprintf(output, "  new submit id %s\n", id)
		go evaluator.RunJudge(&ctx, &pb)
		<-ctx.Running

		if ctx.Status == "completed" && ctx.JudgeResult.Success {
			okCnt++
		} else {
			failCnt++
			fmt.Fprintf(output, "  ! rejudge did not pass - %s/%s now has no valid passing submit\n", old.User, old.Problem)
		}

		// Rebuild Best* from the (now small) set of valid submits for this
		// (user, problem). Handles both rankupdate=best (picks current max)
		// and rankupdate=always (picks latest valid) uniformly.
		if err := dbService.RecomputeUserProblemBest(old.User, old.Problem, &pb); err != nil {
			fmt.Fprintf(output, "  ! failed to recompute Best* for %s/%s: %v\n", old.User, old.Problem, err)
		}
	}

	fmt.Fprintf(output, "\nRejudge complete: %d passed, %d failed/cleared.\n", okCnt, failCnt)
	return nil
}
