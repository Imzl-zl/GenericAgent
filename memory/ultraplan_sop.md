# GA UltraPlan SOP

UltraPlan is GA's controller-driven protocol for work that is too broad, adversarial, or uncertain for a single linear agent pass.

The core model is **not** "one script equals one MapReduce". A script is a **bounded, file-backed MapReduce program**: it may contain multiple phases, multiple fan-out/fan-in passes, and multiple per-item chains, but it should only cover the portion of the task that is currently plannable. When the script finishes, UltraPlan is not automatically finished. The main agent must read the outputs, reduce them into task state, and decide whether to stop, act directly, ask the user, or launch the next UltraPlan script.

```text
main agent controller
  -> write/run a bounded UltraPlan script
  -> read file-backed outputs
  -> reduce into shared state / decision
  -> stop, direct edit, ask, or write the next script
```

## Non-negotiable principles

1. **Script completion is not task completion.** All phases being done only means the currently planned program returned evidence.
2. **Plan only the currently plannable horizon.** If later work depends heavily on unknown results, stop after producing those results and let the main agent regenerate the next script.
3. **Everything important is file-backed.** Subagents return `.out.txt` files (the library manages their output dir); shared state lives in explicit files you create under `temp/`, never in the code tree. The exact directory does not matter; the wiring does.
4. **The main agent owns global reduction.** Subagents produce local evidence, attempts, critiques, or transforms. The main agent owns cross-run state, prioritization, final decisions, and user-facing claims.
5. **Use independent perspectives on purpose.** Parallel agents should not be redundant copies unless redundancy itself is the experiment.
6. **No silent completeness claims.** If coverage is bounded, sampled, timed out, or intentionally skipped, say so.
7. **Scale to the request, not to ambition.** A casual ask gets a few agents and single-pass judgement; "exhaustively audit / be thorough" earns a larger pool and 3-5-vote adversarial verification. Heavy fan-out is opt-in by the request, never inferred.

## Workflow script contract

A GA UltraPlan script is plain Python using `../assets/ga_ultraplan.py`:

```python
from assets.ga_ultraplan import phase, parallel, mapchain
```

Run scripts from the GA code root so `assets` imports correctly. Express orchestration only through `phase()`, `parallel()`, and `mapchain()`; design prompts, shared files, and reduction steps around those primitives.

Put the script file itself and every artifact it writes under `temp/` (or a user-specified area), never in the code tree (root, `assets/`, `memory/`). Running from the root is only for the import to resolve — it is not a license to drop the script there. If a `temp/` script needs the import, add the root to `sys.path` inside it; do not move the script into the code tree to make the import convenient.

Each task can be one of:

```python
("Short display name", "Prompt template with {item}, {previous}, ...")

{
    "desc": "Short display name with {item}",
    "prompt": "Prompt template",
    "data": {"extra": "values"},
    "llm_no": 0,
    "timeout": 3600,
}

lambda: ("Short display name", "Prompt generated lazily at runtime")
```

A task returns the path of its subagent output file, normally `.../temp/ultra_xxx.out.txt`. Treat that path as the workflow value. Do not rely on subagents producing valid JSON unless a later Python step explicitly validates and repairs it.

## Shared workspace: give independent agents just enough shared context

Subagents are independent. A prompt alone is often too narrow: it lacks the evolving global view and encourages duplicated work. For any serious UltraPlan run, put the few shared files it needs under a task folder like `temp/<task-slug>/` (or a user-specified area), never in the code tree. The library auto-creates its own per-run output dir for `.out.txt` files; you do not manage or pass it.

Do not impose a fixed schema or fixed filenames. Name shared files by the role they play in this task: context, item list, prior attempts, target, criteria, frontier, accepted findings. One context file may be enough; a Sweep may need an item list; a Hunt may need prior attempts; an Improve run may need the target plus criteria.

Rules:

- The controller creates and refreshes shared files before launching a script.
- Worker prompts say exactly which shared file(s) to read and which target item, lens, or shard they own.
- Workers may write scratch notes only when explicitly assigned a unique file. Avoid concurrent appends to the same file.
- Reducers read worker `.out.txt` files plus the relevant shared files; they do not infer global truth from one worker.
- Between scripts, the main agent updates whichever notes are useful: tried attempts, accepted findings, rejected claims, remaining frontier, coverage bounds, or changed priorities.
- A worker shares no context with you or its siblings; its prompt must stand alone. Hand it pointers, not payloads: pass a **file path** for anything large or unstructured and let it Read; pass small structured data (an item, a lens, a config) **directly** in the prompt. Never spill a long blob into the prompt, and never write a side-channel file just to smuggle a value a direct argument could carry. Its returned text IS the data, not a message to a human.
- Pure data shaping — dedup, flatten, filter, sort, count — is plain controller code, not an agent. Spend an agent only where judgement is required.

## Primitive semantics

### `phase(name, desc="")`

Use `phase` as a visible boundary in the HTML phase tree and as a semantic boundary in the program. A phase should describe what is being learned, attempted, or reduced.

```python
with phase("Explore target surface", "map unknown areas before choosing attacks"):
    maps = parallel([...])
```

Good phase names are verbs tied to the archetype: `Hunt candidates`, `Verify hits`, `Critique improvement angles`, `Sweep item list`, `Reduce frontier`.

### `parallel(tasks, max_workers=None, **data)`

Run independent tasks concurrently and return output-file paths in input order. This is a barrier: the next Python statement runs only after all tasks complete.

Use `parallel` when a reduction truly needs the full set:

- brainstorming diverse candidates before choosing the best;
- independent sharp critics attacking the same target from different angles;
- partitioned sweep results that must be deduplicated;
- a judge/reducer comparing mutually exclusive approaches.

Do not insert a barrier merely because stages have different names. If each item can move through its stages independently, use `mapchain`.

### `mapchain(items, *steps, max_workers=None, **data)`

Run each item through all steps independently. There is no cross-item barrier between stages: item A can be in verification while item B is still being inspected.

Each step receives the current value as `{item}` and `{previous}`. Initially this is the original item; after each subagent task it becomes that subagent's `.out.txt` path.

When a later step consumes a prior `.out.txt`, instruct it to **tail** that file, not read it, unless the full transcript matters. `.out.txt` files contain the previous worker's full transcript; the useful conclusion is usually near the end.

```python
reports = mapchain(
    items,
    ("Inspect {item}", "Read this run's task folder context, then inspect assigned item: {item}. Output evidence-backed findings."),
    ("Verify {previous}", "Tail {previous}. Try to refute or reproduce the finding with tools."),
)
```

Use `mapchain` by default for per-item multi-stage work. It keeps item context local, avoids artificial barriers, and prevents the main agent from hand-managing intermediate maps.

## Controller cycle: UltraPlan is recursive MapReduce, not a single run

A full UltraPlan often needs several scripts. Each script is one bounded program over the currently knowable horizon; the main agent is the controller between programs.

Use this cycle:

1. **Scout cheaply in the main agent.** Read the request, obvious files, target pages, logs, or prior notes. Do not spawn blind fan-out before identifying the target object.
2. **Create or refresh only the needed shared notes.** Use task-shaped files such as a target/context note, criteria, attempt history, item list, or current findings. Do not assume a universal schema.
3. **Choose the current archetype.** Hunt, Improve, Explore, or Sweep. A script may contain multiple phases and multiple MapReduce barriers, but it should have one dominant purpose.
4. **Write and run the bounded script.** Encode the control flow up front using `phase`, `parallel`, and `mapchain`.
5. **Read outputs physically.** Read the final reducer and any important intermediate `.out.txt` files. Do not trust file names, status lines, or success messages.
6. **Reduce into controller state.** Update the useful shared notes: what was tried, what survived verification, what remains unknown, what coverage is bounded, and what changed priority.
7. **Decide next action.** Stop and answer, edit directly, ask the user, or launch a new UltraPlan script with a new archetype.

This is the key distinction: all phases in one script being complete only ends that script's MapReduce program. It does not prove the overall task is complete.

You do not need the full shape before the *task* — only before the next *orchestration step*. Stay in the loop: run one well-scoped fan-out, read its result, then decide the next phase, rather than committing the entire expand→reduce→verify graph up front. Several small scripts read end-to-end beat one ambitious script that guessed wrong.

## Four recursive archetypes

These are not a fixed catalog of examples. They are four first-principles control shapes that can transform into one another. A serious task often moves from one archetype to another across scripts.

### 1. Hunt: goal-directed search until one viable success is found

Use when the task has a clear success condition and many possible routes. You need broad ideation, non-duplicate attempts, and fast validation.

Examples: find a working exploit path, find any viable implementation approach, find one source that proves a claim, find a way around an integration blocker.

Control shape:

1. Expand the candidate frontier from multiple independent lenses.
2. Deduplicate candidates against the recorded attempt history or other relevant shared notes.
3. Try candidates in parallel or with `mapchain(candidate -> attempt -> verify)`.
4. Stop early only if success is physically verified; otherwise update the frontier and launch the next hunt wave.

Prompt requirements:

- Each hunter must state its lens and avoid repeating recorded attempts.
- Each attempt must specify exact success evidence.
- Verifiers should default to "not proven" unless they can reproduce or directly inspect the evidence.

### 2. Improve: optimize a target by finding sharp defects or upgrade angles

Use when an object exists and the goal is to make it better. Independence is critical: issue-finders must be pointed, skeptical, and non-colluding, not generic reviewers.

Examples: improve a design, harden a plan, optimize prompts, reduce latency, refine an SOP, improve UX, raise code quality.

Control shape:

1. Freeze the current target object in a concrete shared note or file reference.
2. Launch independent critics with distinct sharp lenses: correctness, missing cases, simplicity, performance, security, user value, maintainability, originality, verification.
3. Reduce critiques into a prioritized issue list with evidence.
4. Launch a second script to sweep fixes or design specific remedies.
5. Verify that each accepted improvement actually improves the target and does not break constraints.

Prompt requirements:

- Critics should be adversarial and specific, not balanced reviewers.
- A critique is useful only if it names the exact target fragment, failure mode, and improvement direction.
- The reducer must reject duplicate, vague, or unsupported criticism.

### 3. Explore: determine what exists, what matters, or whether the target is real

Use when the objective itself is uncertain: you do not yet know the landscape, relevant files, possible mechanisms, or whether a phenomenon exists.

Examples: understand an unfamiliar codebase area, investigate a bug class, discover whether a site exposes an API, map a research topic, determine whether a feature already exists.

Control shape:

1. Partition the unknown space into independent lenses or regions.
2. Have explorers gather evidence, not final answers.
3. Reduce into a map of knowns, unknowns, contradictions, and promising next questions.
4. Convert the next step into Hunt, Improve, or Sweep once the target becomes concrete.

Prompt requirements:

- Explorers must report scope covered and scope not covered.
- Reducers must distinguish evidence, inference, and speculation.
- Exploration should produce a frontier, not pretend to close the task.

### 4. Sweep: enumerate and process every item without duplication or omission

Use when the main risk is coverage: every file, row, endpoint, finding, document, or case must be considered.

Examples: per-file migration planning, verify every reported issue, process every source, audit every route, update every usage site.

Control shape:

1. Build an item list and validate the enumeration method.
2. Use `mapchain` for per-item work: inspect -> transform/plan -> verify.
3. Reduce item outputs into a coverage table.
4. Run a gap-checker against the item list and any relevant attempt/coverage notes.
5. If gaps remain, launch another sweep over the missing set.

Prompt requirements:

- Each worker owns exactly one item or shard.
- Each output must include item ID, action taken/proposed, evidence, and unresolved risk.
- Reducers must report coverage count and omissions explicitly.

## Archetype transformations

The four archetypes recursively call each other:

- An **Improve** run may discover unknown areas, causing an **Explore** sub-run.
- An **Explore** run may identify a success route, causing a **Hunt**.
- A **Hunt** that finds a candidate may require **Improve** to harden it.
- Any accepted plan may require **Sweep** to apply or verify it across every item.
- A **Sweep** may expose failures, returning to **Hunt** for workarounds or **Improve** for fixes.

Do not force all transformations into one script. If the next archetype depends on the reducer's result, stop the script, update shared state, and write a new script.

## Script templates by archetype

These templates teach **how to decompose the task**, not how to wire files. Read each one for its lens choices and why those lenses are independent. File wiring is deliberately reduced to a one-line task-folder header (`ctx`, `attempts`, ...); every worker is assumed to read the relevant shared file, so prompts state only the *thinking*, not `Read X and Y`. Run from the GA code root. Keep scripts short enough that the controller can read outputs and decide the next archetype.

### Hunt template

The decomposition idea: **cut the candidate space with several non-overlapping blades** so the union covers more ground than any single hunter would, then dedupe, attempt, and verify.

```python
from assets.ga_ultraplan import phase, parallel

# --- file wiring (shared notes under a task folder) ---
work = "temp/<task-slug>"
ctx, attempts, criteria = f"{work}/context.md", f"{work}/attempts.md", f"{work}/criteria.md"

# --- task decomposition ---
# Three independent blades on the candidate space; each excludes recorded repeats.
hunters = [
    ("Known-pattern hunter", "candidates from established patterns only"),
    ("Weird-angle hunter",   "unconventional candidates; argue why each could work"),
    ("Constraint hunter",    "candidates that exploit the constraints / narrow success condition"),
]

with phase("Expand candidate frontier", "independent lenses, avoid recorded duplicates"):
    notes = parallel([
        (name, f"Read {ctx}, {attempts}, {criteria}. Propose {lens}.") for name, lens in hunters
    ], max_workers=3)

with phase("Reduce candidates", "dedupe and pick what to attempt"):
    shortlist = parallel([
        ("Candidate reducer", f"Read {attempts} and outputs {notes}. Emit a deduped attempt list, each with explicit success evidence.")
    ])[0]

with phase("Attempt and verify", "strongest candidates, physical proof only"):
    results = parallel([
        ("Attempt + verify", f"Read {shortlist}. Try the strongest candidates; report physical evidence of success or failure per candidate.")
    ])

print(results)
```

The three blades above (`known-pattern / weird-angle / constraint`) are the **most generic** placeholder. They are not the answer for every Hunt. After scouting, **re-instantiate the lenses from the domain's real structure**: the blades should encode *competing hypotheses about where this specific problem is hard*, so the reducer can kill the wrong hypothesis from evidence and concentrate effort on the live one. If your three lenses could apply unchanged to any task, you have not decomposed yet. When candidates need *attempt → verify per item*, the attempt phase is a `mapchain(shortlist, attempt_step, verify_step)`, not a flat `parallel`.

### Improve template

```python
from assets.ga_ultraplan import phase, parallel

# wiring: shared notes under a task folder
work = "temp/<task-slug>"
target, criteria, findings = f"{work}/target.md", f"{work}/criteria.md", f"{work}/findings.md"

# decomposition: four non-overlapping critical angles, each hunting a distinct failure class
critics = [
    ("Correctness critic", "exact correctness failures, citing target fragments"),
    ("Simplicity critic",  "over-complex or confused parts; propose a simpler structure"),
    ("Verification critic","claims or changes that lack executable verification"),
    ("Originality critic", "derivative, generic, or weak ideas; propose stronger framing"),
]

with phase("Sharp independent critique", "specific failure modes, not generic review"):
    critiques = parallel([
        (name, f"Read {target}, {criteria}, {findings}. Find {angle}.")
        for name, angle in critics
    ], max_workers=4)

with phase("Prioritize improvements", "reject vague or duplicate criticism"):
    issue_list = parallel([
        ("Issue reducer", f"Read critiques: {critiques}. Prioritize issues with exact location, evidence, fix direction. Reject duplicates and vague comments.")
    ])[0]

print(issue_list)
```

Usually the next script after this template is a **Sweep** over accepted issues or target sections. For a harder draft, run the critics as **competing rewrites** (each critic emits a full alternative), then a reducer picks the strongest per section.

### Explore template

```python
from assets.ga_ultraplan import phase, parallel

# wiring: shared notes under a task folder
work = "temp/<task-slug>"
context, findings = f"{work}/context.md", f"{work}/findings.md"

# decomposition: four mappers triangulating the same unknown from independent directions
mappers = [
    ("Surface mapper",       "enumerate visible components/files/endpoints/sources and what each proves"),
    ("Mechanism mapper",     "how the relevant mechanism likely works; separate evidence from inference"),
    ("Contradiction mapper", "evidence that contradicts the current assumptions"),
    ("Frontier mapper",      "unknowns whose answers would change the next archetype"),
]

with phase("Map unknown space", "evidence first, no premature final answer"):
    maps = parallel([
        (name, f"Read {context}, {findings}. Investigate: {lens}.")
        for name, lens in mappers
    ], max_workers=4)

with phase("Reduce map", "knowns, unknowns, contradictions, next archetype"):
    frontier = parallel([
        ("Exploration reducer", f"Read {findings} and outputs: {maps}. Write knowns, unknowns, contradictions, and recommended next archetype.")
    ])[0]

print(frontier)
```

### Sweep template

```python
from assets.ga_ultraplan import phase, mapchain, parallel

# wiring: shared notes under a task folder; item list is controller-produced/validated
work = "temp/<task-slug>"
context, criteria, items_file = f"{work}/context.md", f"{work}/criteria.md", f"{work}/items.txt"
items = [l.strip() for l in open(items_file, encoding="utf-8") if l.strip()]

# decomposition: same two-step chain per item (inspect -> verify), every item, none skipped
with phase("Per-item sweep", "no duplicate or omitted items"):
    item_reports = mapchain(
        items,
        ("Inspect {item}", f"Read {context}, {criteria}. Inspect ONLY {{item}}. Output item ID, evidence, proposed action, unresolved risk."),
        ("Verify {previous}", "Tail {previous}. Verify the report against tools/files. Keep only supported claims; note gaps."),
        max_workers=6,
    )

with phase("Coverage reduce", "coverage table and omissions"):
    coverage = parallel([
        ("Coverage reducer", f"Read item list {items_file} and reports: {item_reports}. Produce coverage count, omissions, accepted findings, and next missing-set sweep if needed.")
    ])[0]

print(coverage)
```

If you wrote `reviews = parallel(review tasks); verified = parallel(verify tasks)` but each item only needs its own prior review, rewrite it as `mapchain`. For very large item sets, sweep in **bounded batches** (chunk the list, reduce coverage per batch) so the controller can read results between batches.

## Dynamic fan-out and parsing discipline

Avoid asking workers to return complex JSON task graphs. For dynamic fan-out:

1. Prefer controller scouting to produce a plain item/control list before the script.
2. Or have a planner/reducer write a plain text list file with one item per line.
3. Validate the list with Python before using it for `mapchain`.
4. If the list changes the workflow shape, stop and regenerate the next script instead of contorting the current script.

Only parse small, explicit control data. Rich reasoning belongs in `.out.txt` files and shared markdown files.

When a prompt both interpolates wiring (`f"...{ctx}..."`) and shows a worker a literal `{item}`/`{previous}` placeholder, the f-string will eat the literal braces — escape them as `{{item}}`, or the worker receives a blank.

For loop-until-dry discovery, stop only after K consecutive rounds surface nothing new — a fixed `while count < N` misses the tail. Dedup each round against everything **seen**, not against what was **confirmed**: deduping on confirmed lets judge-rejected items reappear every round, so the loop never converges.

## Output discipline

Worker outputs should end with a compact, easy-to-tail conclusion:

```text
## Conclusion
- Item / lens:
- Evidence inspected:
- Finding or attempt result:
- Confidence / failure reason:
- What should happen next:
```

Reducers should produce controller-ready files:

```text
## Decision state
- Accepted facts:
- Rejected / unproven claims:
- Attempts or decisions recorded:
- Remaining frontier:
- Recommended next archetype:
- Coverage bounds / skipped scope:
```

Downstream prompts should usually say `Tail <previous>.out.txt` rather than `Read <previous>.out.txt` to avoid re-ingesting the full transcript noise.

## Final user-facing answer requirements

When UltraPlan is used to produce a plan, the final answer must include:

- what archetype(s) were run and what scope was covered;
- the concrete decision or plan, not just that subagents completed;
- ordered steps with responsible files/items when implementation is involved;
- evidence or output files that support the key claims;
- verification steps that can actually be executed;
- risks, skipped scope, unresolved questions, and why they do or do not block progress;
- whether another UltraPlan wave is recommended.

Do not claim exhaustive coverage unless the item list, coverage notes, and reducer prove it. Do not pad with generic advice.

## Safety and execution rules

- Read relevant code, files, pages, logs, or records before planning edits. UltraPlan must be grounded in the actual target.
- Keep irreversible operations out of worker prompts unless the user explicitly approved them.
- Subagents have full tools and physical execution ability; give precise boundaries, allowed targets, and stop conditions.
- Prefer file paths over copied long text in prompts. Tell workers exactly which shared files and output files to inspect.
- After a script completes, the main agent must read the final output file and sanity-check important intermediate files.
- Use several smaller scripts when the next phase depends on prior results; do not hide recursive control inside one huge script.
- If a worker fails, inspect its `.out.txt` or error, update state, reduce scope or timeout, and retry with new information. Do not repeat blindly.
- If coverage is bounded by time, sampling, top-N, timeout, excluded subsystem, or failed tool, record the bound in the relevant shared note and the final answer.

## Minimal controller skeleton

```python
from assets.ga_ultraplan import phase, parallel, mapchain

# wiring: shared notes under a task folder
work = "temp/<task-slug>"
ctx, attempts, criteria = f"{work}/context.md", f"{work}/attempts.md", f"{work}/criteria.md"

with phase("Current archetype", "state what this script is trying to learn or reduce"):
    outputs = parallel([
        ("Lens A", f"Read {ctx}, plus {attempts} or {criteria} if relevant. Do the assigned independent work."),
        ("Lens B", f"Read {ctx}, plus {attempts} or {criteria} if relevant. Use a different lens; avoid duplicating Lens A."),
    ])

with phase("Reduce", "controller-ready decision state"):
    reduced = parallel([
        ("Reducer", f"Read the relevant shared notes and these outputs: {outputs}. Write accepted facts, rejected claims, frontier, and recommended next archetype.")
    ])[0]

print(reduced)
```

After this script exits, the main agent reads `reduced`, updates shared state, and decides whether to answer, act directly, ask the user, or write the next UltraPlan script.
