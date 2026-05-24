package judge

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"sync"

	"github.com/mrhaoxx/SOJ/types"
	"github.com/rs/zerolog/log"
)

// JudgeRunner is implemented by Evaluator and kept small so the queue can be
// tested without launching containers.
type JudgeRunner interface {
	RunJudge(ctx *types.SubmitCtx, problem *types.Problem)
}

type queueJob struct {
	ctx     *types.SubmitCtx
	problem types.Problem
}

// Scheduler runs submissions one at a time and keeps in-process log streams
// available for attach sessions.
type Scheduler struct {
	cfg      *types.Config
	db       *types.DatabaseService
	runner   JudgeRunner
	problems map[string]types.Problem

	queue chan queueJob

	mu      sync.Mutex
	active  string
	streams map[string]*submissionStream
}

// NewScheduler creates and starts the exclusive queue worker.
func NewScheduler(cfg *types.Config, db *types.DatabaseService, runner JudgeRunner, problems map[string]types.Problem) *Scheduler {
	s := &Scheduler{
		cfg:      cfg,
		db:       db,
		runner:   runner,
		problems: problems,
		queue:    make(chan queueJob, 1024),
		streams:  make(map[string]*submissionStream),
	}
	go s.worker()
	return s
}

// RecoverQueued loads queued submissions left by a previous process and puts
// them back into the in-memory queue.
func (s *Scheduler) RecoverQueued() error {
	submits, err := s.db.GetQueuedSubmits()
	if err != nil {
		return err
	}
	for i := range submits {
		submit := submits[i]
		problem, ok := s.problems[submit.Problem]
		if !ok {
			submit.SetStatus("dead").SetMsg("problem definition not found during queue recovery")
			if err := s.db.UpdateSubmit(&submit); err != nil {
				return err
			}
			continue
		}
		s.enqueueCtx(&submit, problem)
	}
	return nil
}

// Enqueue registers a newly-created queued submission. The returned number is
// the current number of submissions ahead of it, including the active one.
func (s *Scheduler) Enqueue(ctx *types.SubmitCtx, problem types.Problem) (int64, error) {
	ahead, err := s.db.CountSubmitsAhead(ctx.SubmitTime)
	if err != nil {
		return 0, err
	}
	s.enqueueCtx(ctx, problem)
	return ahead, nil
}

func (s *Scheduler) enqueueCtx(ctx *types.SubmitCtx, problem types.Problem) {
	ctx.SubmitDir = path.Join(s.cfg.SubmitsDir, ctx.User, ctx.Problem)
	ctx.Workdir = path.Join(s.cfg.SubmitWorkDir, ctx.ID)
	ctx.RealWorkdir = path.Join(s.cfg.RealSubmitWorkDir, ctx.ID)
	ctx.Running = make(chan struct{})
	s.ensureStream(ctx.ID)
	s.queue <- queueJob{ctx: ctx, problem: problem}
}

func (s *Scheduler) worker() {
	for job := range s.queue {
		stream := s.ensureStream(job.ctx.ID)
		job.ctx.Userface = types.Userface{
			Buffer: bytes.NewBuffer(nil),
			Writer: stream,
		}
		job.ctx.Running = make(chan struct{})

		s.mu.Lock()
		s.active = job.ctx.ID
		s.mu.Unlock()

		job.ctx.SetStatus("running")
		if err := s.db.UpdateSubmit(job.ctx); err != nil {
			log.Error().Err(err).Str("submit", job.ctx.ID).Msg("failed to mark submit running")
		}

		s.runner.RunJudge(job.ctx, &job.problem)

		if err := s.db.UpdateUserSubmitResult(job.ctx.User, job.ctx, &job.problem); err != nil {
			log.Error().Err(err).Str("user", job.ctx.User).Str("submit", job.ctx.ID).Msg("failed to update user submit result")
		}

		stream.Close()

		s.mu.Lock()
		if s.active == job.ctx.ID {
			s.active = ""
		}
		s.mu.Unlock()
	}
}

// Attach writes buffered logs and then follows future logs until the
// submission reaches a terminal state or the writer returns an error.
func (s *Scheduler) Attach(submitID string, w io.Writer) error {
	stream := s.ensureStream(submitID)
	ch, backlog, done := stream.Subscribe()
	defer stream.Unsubscribe(ch)

	if len(backlog) > 0 {
		if _, err := w.Write(backlog); err != nil {
			return err
		}
	}

	for {
		select {
		case p, ok := <-ch:
			if !ok {
				return nil
			}
			if _, err := w.Write(p); err != nil {
				return err
			}
		case <-done:
			for {
				select {
				case p, ok := <-ch:
					if !ok {
						return nil
					}
					if _, err := w.Write(p); err != nil {
						return err
					}
				default:
					return nil
				}
			}
		}
	}
}

func (s *Scheduler) QueueAhead(submit *types.SubmitCtx) (int64, error) {
	return s.db.CountSubmitsAhead(submit.SubmitTime)
}

func (s *Scheduler) ensureStream(submitID string) *submissionStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	stream, ok := s.streams[submitID]
	if !ok {
		stream = newSubmissionStream()
		s.streams[submitID] = stream
	}
	return stream
}

type submissionStream struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	subs map[chan []byte]struct{}
	done chan struct{}
}

func newSubmissionStream() *submissionStream {
	return &submissionStream{
		subs: make(map[chan []byte]struct{}),
		done: make(chan struct{}),
	}
}

func (s *submissionStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, err := s.buf.Write(p)
	if err != nil {
		return n, err
	}
	for ch := range s.subs {
		cp := append([]byte(nil), p...)
		select {
		case ch <- cp:
		default:
			log.Warn().Msg("dropping attach log chunk for slow subscriber")
		}
	}
	return len(p), nil
}

func (s *submissionStream) Subscribe() (chan []byte, []byte, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch := make(chan []byte, 256)
	s.subs[ch] = struct{}{}
	return ch, append([]byte(nil), s.buf.Bytes()...), s.done
}

func (s *submissionStream) Unsubscribe(ch chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.subs, ch)
	close(ch)
}

func (s *submissionStream) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.done:
		return
	default:
		close(s.done)
	}
}

func (s *Scheduler) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprintf("Scheduler(active=%q)", s.active)
}
