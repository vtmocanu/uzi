---
slug: test-improvement
name: Weekly test improvement
description: Weekly pass that finds one under-tested area and strengthens its tests.
target: prompt
cron: 0 8 * * 1
timezone: UTC
---

Spend this run improving the project's automated tests. Pick ONE area that is
meaningfully under-tested — a module with thin coverage, an important branch with
no assertion, or a bug-prone path — and add focused, genuinely useful tests for
it. Prefer a small number of high-value tests over many shallow ones, and run the
project's test suite to confirm your additions pass.

Make every test earn its place:

- Prove each assertion is non-vacuous by identifying a plausible defect that
  would make it fail; if you sanity-check by mutating the code under test, do it
  in a throwaway copy and never modify a production file on your working branch.
  Choose the mutation well — deleting the code under test, blanking a field, or
  changing it to a brand-new sentinel proves little (any comparison catches it);
  change it to a value another case already produces.
- Assert the observable end-state, not an intermediate call: check the value or
  effect a caller would actually see, rather than that some helper was invoked.
- Prefer positive assertions over negative ones — assert what the result IS, not
  merely what it is not; a negative assertion passes for many wrong reasons.
- Never weaken or delete an existing assertion to make a suite pass. If a test
  fails, understand why; do not loosen it to go green.
- Do not re-touch a test another recent run already changed — pick a different
  area so parallel runs do not collide.

Guardrail: change TEST files only — do NOT modify any production (non-test) file.
If a behavior needs a production change to be testable (a new seam, an extracted
function), that is out of scope — pick another area. If you find a real production
bug while testing, report it rather than fixing it; never change production code
to make a test pass. Commit your new tests and open a merge request.

If nothing worthwhile comes up this week, an empty week is acceptable — do NOT
invent low-value tests just to hit a number. Make no change and open no merge
request: leave a short note on what you looked at.
