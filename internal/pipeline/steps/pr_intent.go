package steps

import (
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// publicPRIntent returns the intent text published on the PR body, or "" when
// publication is suppressed. It is the single publication gate: every PR-body
// render path (ordinary drafting, fallback content, and template appendix)
// goes through it, so caller-side omission, the repository's trusted policy,
// and the foreign-owner rule compose here and nowhere else.
//
// Every control is tighten-only:
//
//   - The repository's trusted pr.publish_intent (default true) is the
//     ceiling. A caller can never publish intent on a repository whose
//     trusted config disabled it.
//   - The caller-side decision recorded on the run (runs.omit_intent: the
//     axi run --no-publish-intent flag, or the operator's global
//     intent.publish_intent: false default, folded together at run start)
//     can only remove the section, never restore it.
//   - A foreign-owner PR (isForeignOwnerPR: a fork whose owner differs from
//     the parent's) omits the section whatever the other two allow.
//
// The repository policy never touches the intent that reaches step prompts.
// The caller-side omission is different: it also WITHHOLDS the intent from the
// PR-drafting turns (prDraftIntentPromptSection), because a PR title or body
// drafted with the intent in context can paraphrase it into the public text,
// and there is deliberately no output filter or prose scanner to catch that.
// The foreign-owner rule removes only the section; drafting keeps the intent.
// Review, test, document, lint, and CI-fix prompts keep the full intent
// under every signal.
func publicPRIntent(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.Config != nil && !sctx.Config.PR.PublishesIntent() {
		return ""
	}
	if runOmitsIntent(sctx) || isForeignOwnerPR(sctx) {
		return ""
	}
	return cleanedUserIntent(sctx)
}

// runOmitsIntent reports whether this run was started with the caller-side,
// tighten-only request to keep the generated Intent section out of the PR
// body. The decision is stamped on the run row at start, so it survives
// daemon restarts and reruns.
func runOmitsIntent(sctx *pipeline.StepContext) bool {
	return sctx != nil && sctx.Run != nil && sctx.Run.OmitIntent
}

// prDraftIntentPromptSection is the intent section for the PR-drafting turns
// (ordinary narrative, title-only fallback, and repository-template
// narrative). Under the caller-side omission it is empty, so those turns
// draft from the diff and commit messages only and no intent text can reach
// the public PR through a paraphrase. Every other step prompt keeps
// userIntentPromptSection unchanged.
func prDraftIntentPromptSection(sctx *pipeline.StepContext) string {
	if runOmitsIntent(sctx) {
		return ""
	}
	return userIntentPromptSection(sctx)
}
