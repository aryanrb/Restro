---
name: commit-push
description: Generate a Conventional Commits message from the current changes and push to the GitHub remote. Use when the user says "commit and push", "commit this", "push to github", or invokes /commit-push.
---

# commit-push

Stage (if needed), write a good commit message, commit, and push to GitHub.

## Steps

1. **Inspect** the changes so the message is accurate — never guess:
   - `git status --short` and `git diff --staged` (and `git diff` for unstaged).
   - If nothing is staged, stage the relevant changes with `git add` (respect `.gitignore`;
     never force-add `.env`, secrets, or build output). Confirm `git check-ignore .env`.

2. **Write the message** in Conventional Commits form:
   - Subject: `type(scope): summary` — ≤ 72 chars, imperative mood.
     Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`, `build`, `ci`.
   - Body: a short bullet per meaningful change (what + why), wrapped ~72 cols.
   - Footer (required): end with
     `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`
   - Pass the message via multiple `-m` flags or a heredoc; do not open an editor.

3. **Commit**: `git commit` with the message above.

4. **Push** to the branch's upstream:
   - If an upstream exists: `git push`.
   - If not: `git push -u origin HEAD`.
   - If the remote uses HTTPS and the push fails on auth, stop and tell the user to
     authenticate (`gh auth login`, a PAT, or a credential helper) — never embed a token.

5. **Report**: the commit hash, subject, and the pushed branch/remote.

## Guardrails
- Commit/push only when asked. On a brand-new repo's first commit, `master`/`main` is fine.
  Otherwise, if on the default branch and this is normal feature work, prefer a feature
  branch — unless the user's established pattern is to commit directly.
- Never commit secrets. If a staged file looks like it holds a credential, stop and ask.
- Report test/build status honestly; don't claim green without checking if the change is risky.
