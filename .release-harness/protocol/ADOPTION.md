# Adopting release-harness into a project

This is a protocol, not a script. It tells you — an agent with file access and a
shell — how to help someone author a release contract that is honest about what
it knows.

It is provider-neutral by construction: it names only the public `release-harness`
commands and the two files you edit. Another competent agent, on another host,
following this document, should produce a structurally compatible result.

## What you are doing

Helping the operator arrive at **an intentional accepted proposition**: a written
statement of what must hold of their software, which they have deliberately
agreed to.

You are not certifying anything. You are not deciding whether their software is
good. You are helping them say what "working" means for it, and recording
honestly how you came to believe each part.

**Adoption is complete when an accepted contract exists by digest.** Not when a
run passes — that is a separate action the operator takes afterwards.

## The one rule that matters

> Never write a claim you cannot point at evidence for.

Everything else in this document follows from that. A tool that invents a port,
a framework, or a subject name produces a contract indistinguishable from one
the operator knew to be true — and the operator has no way to tell them apart.
That failure has happened, in this exact product, and it is why this protocol
exists.

If you find yourself about to write something because it is *usually* true, or
because the directory is named a certain way, stop and ask instead.

## The shape of the work

```
inspect → draft → record evidence → resolve questions → validate → review → accept
```

You will move back and forth between the first four. That is normal; authoring
is not linear.

---

## 1. Establish where you are

Before inspecting anything, know your starting point, and say it out loud to the
operator:

```
release-harness doctor
```

If release-harness is not installed, install it:

```
release-harness init
```

`init` creates directories and its own config. It makes no claims about the
project — that is deliberate, and it is your job too.

Then find out what already exists. A repository may have been partly adopted
before, by a person or by another agent:

```
release-harness draft list
```

If a draft and authoring record exist, **read them first**. They are the durable
state of this work. You do not need a transcript of whatever happened before;
the record tells you what was established and what was left open.

## 2. Inspect, with bounds you can state

Look at high-signal sources first. These are the files whose presence and
contents are *declarations of intent*, and they are small enough to read
completely:

- git metadata — remotes, branches, whether this is a repository at all
- package/workspace manifests (`package.json`, `pyproject.toml`, `go.mod`,
  `Cargo.toml`, `pom.xml`, workspace definitions)
- container and orchestration files (`Dockerfile`, `docker-compose*.yml`, k8s
  manifests, `Procfile`)
- CI/CD workflows
- test configuration, and the tests themselves when they assert something about
  shape
- release configuration (publish config, version files, changelogs)
- entry points named by the manifests — not files you guessed at

**Do not run a naive recursive scan.** It is slow, it gets killed on large
repositories, and — the part that actually matters — a scan that was killed
tells you nothing, but *feels* like it told you something. That is the exact
shape of the most common error in this domain.

Prefer targeted inspection: read the manifest, then follow what it names.

### Record the bounds of every search

When you search and find nothing, you must be able to say **where you looked**
and **whether you finished**. If you cannot say both, you have not established
absence — you have established nothing.

This is not bookkeeping. It is the difference between "this project has no
tests" and "I did not find tests," which are different claims with different
consequences.

## 3. Identify candidate subjects

A **subject** is a thing someone would want to certify: something that is
released, deployed, or depended upon as a unit.

Signals that something is a subject:

- it is published (a package with a name and version)
- it is deployed (a service with a Dockerfile, a compose entry, a deploy job)
- it has its own lifecycle (versioned separately, released separately)
- something else depends on it across a boundary

Do not force the operator into a taxonomy. Words like "monorepo", "component" and
"release unit" are fine in conversation, but the machine model has exactly three
things: a **subject**, its **assertions**, and its **normative references**. Get
to those.

When the evidence genuinely does not settle how many subjects there are, that is
a semantic question — ask it (§6).

## 4. Look outward only when evidence points outward

Stay inside the repository unless something in it references something outside:
a dependency on a sibling package, a compose file naming another service, a
workflow deploying to a named environment, a client calling a documented API.

When that happens, say what you found and ask whether it matters:

> `package.json` depends on `@acme/schema` at `^2.0.0`, which isn't in this
> repository. If that schema changing would break this service's promises, it's
> a normative reference and I should record it. Is it?

A normative reference is something whose *change alters what your assertions
mean*. A database you happen to connect to is usually an execution binding. A
schema you must stay compatible with is usually normative. The operator decides;
you surface the candidate.

When the operator confirms one, it goes in `proposition.requires` — an array of
`{ ref, digest }`, where the digest is the sha256 of the accepted contract you
are pinning to:

```json
"requires": [
  { "ref": "example-upstream-contract", "digest": "<64 hex chars>" }
]
```

An unpinned reference is a convention, not a reference, so the digest is
required. If the thing you would pin has no accepted contract yet, say so and
ask — do not invent a digest, and do not record the reference under any other
field name.

## 5. Record what you found, and how you know

Everything you learn goes into the authoring record with an honest status. There
are five, and the distinctions are load-bearing:

| Status | Means | Can an accepted assertion rest on it? |
|---|---|---|
| `observed` | You read it. Cite the file and line. | yes |
| `observed_absent` | A **completed**, bounded search found nothing. Carries method, roots, exclusions. | yes |
| `asserted_absent` | Something asserts it must not exist — a test, a contract. Cite it. | yes |
| `inferred` | You worked it out. A reasonable interpretation, not a fact. | **no** — draft only |
| `not_established` | The search never ran, failed, was killed, timed out, or truncated. | **no** |

Three things to be careful about:

**`inferred` is honest and useful.** Use it freely while drafting. It simply
cannot survive into an accepted contract — acceptance will refuse it, and that
refusal is the mechanism working. Convert it by checking, or by asking.

**`not_established` is not a failure state.** Recording "I could not determine
this" is a genuine contribution. An operator who sees it can answer in one
sentence. An operator who sees a confident guess instead has no idea anything
was uncertain.

**`asserted_absent` is the one people miss.** A file being absent because a test
requires it to be absent is a completely different fact from a file being
missing. If you find a test asserting nonexistence, that assertion is stronger
evidence than any search you could run — it states intent, not just current
state.

## 6. Ask only what you cannot responsibly answer

Every question costs the operator attention. Spend it on things files cannot
settle.

**Do not ask** what a file plainly says. If `docker-compose.yml` maps port 8080,
you know the port. Asking makes you look careless and teaches the operator their
answers do not matter.

**Do ask** about intent, boundaries and meaning:

- What should this subject be called? (You may propose one from the package
  name — but say where it came from, and let them correct it.)
- Are these two packages one promise or two?
- Which behaviour, if broken, would make a release unacceptable?
- Is this dependency normative, or just where it happens to run?
- Is this external surface part of what you're certifying?

Keep the count low. A simple project should feel nearly automatic — one or two
questions. If you have ten, most of them are probably derivable or premature.

Write every question into the draft's `questions` array, with `blocking: true`
when acceptance should not proceed without an answer. A question you asked in
conversation and did not record is a question that will be lost.

When the operator answers one, record both fields:

```json
{
  "id": "Q1",
  "question": "Do these two packages release independently?",
  "blocking": true,
  "resolution": "No — they ship together from one tag.",
  "resolved_by": "a.operator"
}
```

`resolved_by` is required. A resolution nobody is named for cannot be told apart
from an agent answering its own question, and that is a thing you must never do.

## 7. Propose assertions that would actually catch something

This is where you earn your place. The deterministic core cannot tell a
meaningful contract from a vacuous one — `exit_code: 0` is perfectly valid and
often worthless. You can.

For each subject, ask yourself:

> If every one of these assertions passed, what could still be broken badly
> enough that the operator would regret shipping?

Then say that out loud. Common gaps worth naming:

- **A health check proves the process is alive, not that it works.** Nearly
  every adoption starts here, and it is nearly always insufficient on its own.
- **The tested surface is not the deployed surface.** Testing a development
  build while traffic goes to a production alias means the assertions cover
  something nobody uses.
- **The authority lives elsewhere.** If another repository owns the schema you
  validate against, a local test proves only local agreement.
- **The dependency isn't represented.** A service that cannot function without
  another one, with no assertion about that relationship, has a promise it
  cannot keep.

Recommend; do not insist. The operator decides what readiness means. Your job is
to make sure they decide it knowingly rather than by omission.

## 8. Surface contradictions instead of resolving them

You will find sources that appear to disagree. A README describing an endpoint
the router does not have. A workflow that looks authoritative but is never
triggered. Two files that name different targets.

**First, establish whether they actually conflict.** Two sources disagree only
when their claims are incompatible. Different files, different mechanisms, or
different deployment targets are not by themselves a contradiction — a project
may legitimately have a local mirror and a real deployment, or a build target
and a serving target.

Read what each source says about *its own* status. A source may explicitly
disclaim its authority or name its successor; one that defers to another is not
in conflict with it. Recording a contradiction there would be inventing a
conflict, which is the same failure as inventing a fact — and it is easy to get
wrong precisely because a disagreement is what you are looking for.

When sources genuinely do conflict, **do not pick a winner from a rule.** "Code
beats docs" is wrong as often as it is right: a test asserting deliberate
absence encodes intent more strongly than a file listing does, and a deployment
config may be aspirational while the documentation describes what actually runs.

Record both sources and say plainly that they disagree:

```json
{
  "id": "auth-token-lifetime",
  "claim": "access tokens expire after 15 minutes",
  "status": "not_established",
  "evidence": {
    "contradiction": [
      { "source": "docs/api.md:88", "says": "tokens are valid for 15 minutes" },
      { "source": "src/auth/issue.ts:24", "says": "expiresIn: '1h'" }
    ]
  }
}
```

If an assertion depends on the contradiction, it blocks acceptance until the
operator resolves it. If nothing depends on it, record it and move on — a
documented contradiction is a useful artifact.

## 9. Write the files

Two files, both plain JSON, both meant to be read and edited by a human:

```
.release-harness/drafts/<name>.draft.json     what is proposed
.release-harness/drafts/<name>.record.json    how you know
```

Create them with `release-harness draft new <name>`, then edit. Do not invent
your own format or location — these files are the authoring API, and the
operator must be able to take over from you at any point using nothing but a
text editor.

## 10. Validate with the public command

```
release-harness validate --draft <name>
```

Use this, not your own reasoning about whether the files look right. It is the
same check the operator gets, and the same check acceptance will run.

It will tell you two different things, and they are not the same:

- **well-formed** — the files parse and satisfy their schema
- **ready to accept** — nothing blocks acceptance

A draft can be well-formed and nowhere near ready. Report both accurately.
Saying "valid" when you mean "well-formed" is how a fabricated contract once got
described as ready.

## 11. Present the blockers

When acceptance is blocked, show the operator exactly what is unresolved and
what each one needs. Blockers come in three kinds, and they need different
things from the operator:

- an **unresolved question** needs a decision
- an **unsupported claim** (an inference under an accepted assertion) needs
  either a check you can run, or the operator's knowledge
- a **missing claim** means an assertion cites evidence that is not there —
  usually your mistake to fix

Work through them. Record each resolution in the file where it belongs: question
resolutions in the draft, evidence corrections in the record.

## 12. Review before acceptance

When the draft validates and nothing blocks, stop and show the operator what
they are about to agree to:

- the **subject**, and why it is called that
- each **assertion**, in plain language
- any **normative references**, and what depends on them
- the **semantic decisions they made** — their answers, reflected back
- what this contract **does not cover** — the known limitations, the
  `not_established` claims, the assertions you recommended that were declined

That last item is the one to get right. The operator should finish the review
knowing what they are *not* protected against.

## 13. Ask for explicit approval

Acceptance means someone takes responsibility. It must be a decision, not a
side effect.

Ask plainly: *"Shall I accept this proposition on your behalf, attributed to
you?"*

Do **not** treat any of these as approval:

- "looks good" in response to something else
- validation passing
- silence, or a lack of objection
- the files having been written

Then accept through the same command a person would use:

```
release-harness accept --draft <name> --by "<operator>"
```

Attribution names whoever took responsibility, not you. You did the authoring;
they did the accepting.

## 14. Afterwards

Report the digest. Adoption is complete: an intentional accepted proposition
exists.

You may then offer to help create execution bindings:

```
release-harness bind local --target <symbol>=<location>
```

Bindings are not part of the contract — that separation is what lets the same
proposition be checked locally and in CI.

**Do not run a certifying run and report success as part of adoption.** If the
operator wants to certify, that is their next action:

```
release-harness run --binding local
release-harness verify
```

Never say "setup complete and your release passes" unless a real certifying run
was invoked and actually passed.

---

## Things you must not do

- **Do not invent a subject id** from the directory name. It is the field most
  tempting to guess and the one most likely to be wrong.
- **Do not guess ports, URLs, frameworks or entry points.** Read them, or ask.
- **Do not claim absence** from a search you did not finish.
- **Do not resolve a question on the operator's behalf** and mark it resolved.
- **Do not bypass `validate` or `accept`.** There is no agent-only path, by
  design. If you find yourself wanting internal access, the protocol is wrong,
  not the product.
- **Do not certify.** Adoption ends at acceptance.

## The completion checklist

You are done when all of these hold:

1. Bounded inspection performed, and you can state its bounds
2. Material contradictions resolved, or explicitly recorded as non-blocking
3. Blocking semantic questions answered by the operator
4. `release-harness validate` passes
5. The authoring record is honest enough that a stranger could audit it
6. The operator explicitly approved acceptance
7. An accepted contract exists, identified by digest
