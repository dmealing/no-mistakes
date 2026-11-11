package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// SessionRole identifies which durable review-loop session an invocation
// belongs to. The fixer role spans every review-fix turn of a run. The
// reviewer role spans one review PASS - its initial turn plus the finalize
// turn that answers its own open questions - and nothing more: a rereview
// certifies fixes implementing the previous review turn's findings, so
// resuming a review session across a code change would seat the prescriber of
// those fixes as their certifier and degrade the rereview into checking that
// its own prescription was implemented.
type SessionRole string

const (
	// SessionRoleReviewer spans ONE review pass: the initial turn, and the
	// finalize turn that resumes it once the operator has answered every
	// question that turn left open. It never spans a code change - the review
	// step Forgets it before any fix round, and a new run gets a new manager -
	// so a reviewer is never seated as the certifier of its own prescription.
	// See docs/src/content/docs/concepts/review-conversation.md.
	SessionRoleReviewer SessionRole = "reviewer"
	SessionRoleFixer    SessionRole = "review-fixer"
)

// SessionColdAfter is how long a durable session may sit idle before its
// provider's prompt cache is treated as expired. Resuming a session after that
// re-sends its whole history as uncached input: a measured Sonnet fixer that
// resumed a cold session cost 4.5 quota units against 2.6 for a fresh one, so a
// fresh session handed a short summary of the earlier rounds is cheaper and
// loses nothing the round history does not already carry.
const SessionColdAfter = 5 * time.Minute

// RunSessions manages the per-run, per-role durable agent sessions of the
// review loop. It is strictly scoped to one run: identities are keyed by
// (run, role), persisted as minimum resume metadata (run, role, agent,
// session id - never prompts or transcripts), and never shared across runs,
// branches, repositories, or roles.
//
// Correctness always wins over reuse: adapters without session support run
// cold, a failed resume drops the identity and re-runs the same turn in a
// fresh same-role session, and any persistence failure degrades to cold
// invocations. A nil *RunSessions runs everything cold, preserving the
// pre-session behavior for steps outside the review loop and for tests.
type RunSessions struct {
	db      *db.DB
	runID   string
	agent   agent.Agent
	enabled bool

	mu  sync.Mutex
	ids map[SessionRole]agent.SessionRef
	// lastUsed is when each role's session last finished a turn, which is
	// when the provider last wrote its prompt cache. Persisted as the row's
	// updated_at so a daemon restart still knows how cold a session is.
	lastUsed map[SessionRole]time.Time
	now      func() time.Time
}

// NewRunSessions creates the manager for one run, loading any persisted
// session identities recorded by a previous process for the same run and
// agent. Identities stored for a different adapter are ignored: a session id
// is only meaningful to the adapter that minted it.
func NewRunSessions(database *db.DB, runID string, sessionAgent agent.Agent, enabled bool) *RunSessions {
	rs := &RunSessions{
		db:       database,
		runID:    runID,
		agent:    sessionAgent,
		enabled:  enabled,
		ids:      map[SessionRole]agent.SessionRef{},
		lastUsed: map[SessionRole]time.Time{},
		now:      time.Now,
	}
	if database != nil {
		if stored, err := database.GetRunAgentSessions(runID); err == nil {
			for _, s := range stored {
				if s.SessionID != "" && agent.SupportsSessionProvider(sessionAgent, s.Agent) {
					rs.ids[SessionRole(s.Role)] = agent.SessionRef{ID: s.SessionID, Agent: s.Agent}
					rs.lastUsed[SessionRole(s.Role)] = time.Unix(s.UpdatedAt, 0)
				}
			}
		}
	}
	return rs
}

// SetClock replaces the clock the cold-session check reads. Tests use it to
// age a session without sleeping.
func (rs *RunSessions) SetClock(now func() time.Time) {
	if rs == nil || now == nil {
		return
	}
	rs.mu.Lock()
	rs.now = now
	rs.mu.Unlock()
}

// DropColdSession forgets role's stored session when it has sat idle longer
// than SessionColdAfter, so the next turn starts a fresh session instead of
// resuming one whose prompt cache has expired. It reports how long the session
// sat idle and whether it was dropped. A session whose last use is unknown is
// treated as cold. A nil or disabled manager has nothing to drop.
func (rs *RunSessions) DropColdSession(role SessionRole) (time.Duration, bool) {
	if rs == nil || !rs.enabled {
		return 0, false
	}
	rs.mu.Lock()
	_, stored := rs.ids[role]
	last := rs.lastUsed[role]
	idle := rs.now().Sub(last)
	rs.mu.Unlock()
	if !stored || (!last.IsZero() && idle <= SessionColdAfter) {
		return 0, false
	}
	if last.IsZero() {
		idle = 0
	}
	rs.forget(role)
	return idle, true
}

// StartsFresh reports whether role's next turn on a will begin a new session
// rather than resume one: there is no manager, reuse is off, the adapter
// cannot resume, or no identity is stored for the role.
func (rs *RunSessions) StartsFresh(a agent.Agent, role SessionRole) bool {
	if rs == nil || !rs.enabled || !agent.SupportsSessionResume(a) {
		return true
	}
	return rs.id(role).ID == ""
}

// Run executes one turn of the given role, reusing the role's durable
// session when the adapter supports it. logf (optional) receives operator-
// visible notes about session reuse and fallbacks.
func (rs *RunSessions) Run(ctx context.Context, a agent.Agent, role SessionRole, opts agent.RunOpts, logf func(string)) (*agent.Result, error) {
	if rs == nil || !rs.enabled || !agent.SupportsSessionResume(a) {
		if rs != nil && rs.enabled && logf != nil {
			logf(fmt.Sprintf("agent %s does not support session resume; running cold", a.Name()))
		}
		return a.Run(ctx, opts)
	}

	stored := rs.id(role)
	storedID := stored.ID
	opts.Session = &stored
	result, err := a.Run(ctx, opts)
	if err == nil {
		rs.remember(role, result.SessionID, sessionProvider(a, result))
		return result, nil
	}
	if storedID == "" || ctx.Err() != nil || (result != nil && result.ResumeRetryable != nil && !*result.ResumeRetryable) {
		return nil, err
	}

	// The resume attempt failed. Never skip the turn: drop the dead identity
	// and re-run the same turn in a fresh same-role session.
	if logf != nil {
		logf(fmt.Sprintf("resume of %s session failed (%v); starting a fresh %s session", role, err, role))
	}
	rs.forget(role)
	opts.Session = &agent.SessionRef{}
	opts.SessionFallback = true
	opts.SessionFallbackReason = classifyFallbackReason(err)
	if opts.OnLifecycle != nil {
		opts.OnLifecycle(agent.LifecycleEvent{
			Agent:   a.Name(),
			Phase:   agent.LifecyclePhaseFallback,
			Message: fmt.Sprintf("%s session resume failed; starting a fresh %s session", a.Name(), role),
		})
	}
	result, err = a.Run(ctx, opts)
	if err != nil {
		return nil, err
	}
	rs.remember(role, result.SessionID, sessionProvider(a, result))
	return result, nil
}

func (rs *RunSessions) id(role SessionRole) agent.SessionRef {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.ids[role]
}

// remember stores the role's latest session identity in memory and persists
// it so the run can resume the session across daemon process boundaries.
// Persistence failures are ignored: reuse degrades, correctness does not.
func (rs *RunSessions) remember(role SessionRole, sessionID, provider string) {
	if sessionID == "" {
		return
	}
	if provider == "" || !agent.SupportsSessionProvider(rs.agent, provider) {
		return
	}
	identity := agent.SessionRef{ID: sessionID, Agent: provider}
	rs.mu.Lock()
	rs.ids[role] = identity
	rs.lastUsed[role] = rs.now()
	rs.mu.Unlock()
	// Upsert even when the identity is unchanged: the write refreshes the
	// row's updated_at, which is how a restarted daemon judges the session's
	// cache age.
	if rs.db != nil {
		_ = rs.db.UpsertRunAgentSession(rs.runID, string(role), provider, sessionID)
	}
}

func sessionProvider(a agent.Agent, result *agent.Result) string {
	if result != nil && result.Provider != "" {
		return result.Provider
	}
	if a == nil {
		return ""
	}
	return a.Name()
}

// Forget drops a role's session identity, in memory and in the durable store,
// so the role's next turn starts cold.
//
// The review loop needs it for the independence guarantee: a reviewer session
// may span the questions and answers of ONE pass, but never a code change. The
// review step calls this before any fix round, so the turn that judges a fix
// can never be the session that prescribed it. A nil manager is a no-op, which
// is already cold.
func (rs *RunSessions) Forget(role SessionRole) {
	if rs == nil {
		return
	}
	rs.forget(role)
}

func (rs *RunSessions) forget(role SessionRole) {
	rs.mu.Lock()
	delete(rs.ids, role)
	delete(rs.lastUsed, role)
	rs.mu.Unlock()
	if rs.db != nil {
		_ = rs.db.DeleteRunAgentSession(rs.runID, string(role))
	}
}
