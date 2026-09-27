_🩺 Stability & Availability_ | _🟠 Major_ | _⚡ Quick win_

<details>
<summary>🔎 Supported by static analysis</summary>

🏁 Script executed:

```bash
cat api/internal/store/migrations/00257_recovery_wait_vault_locked.sql
rg -n 'runs_recovery_wait_cause_check|NOT VALID|NO TRANSACTION' api/internal/store/migrations | head -50
```

Repository: vtmocanu/uzi

Length of output: 8307

---

🏁 Script executed:

```bash
set -eu
printf '%s\n' '--- migration transaction annotations and runner references ---'
rg -n -C 3 'NO TRANSACTION|goose|BeginTx|RunMigrations|RunContext|Transaction' --glob '!vendor/**' --glob '!node_modules/**' .
```

Repository: vtmocanu/uzi

Length of output: 45552

</details>

**Split constraint validation into a separate migration.**

`00257` runs transactionally because it has no `NO TRANSACTION` annotation. Adding `NOT VALID` and validating it in the same file would not reduce blocking.

Use the repository's staged pattern. Add both constraints as `NOT VALID` in `00257`, then validate the widened constraint in a following migration.

<details>
<summary>Suggested fix</summary>

```diff
-    ));
+    )) NOT VALID;
```

</details>

<!-- fingerprinting:phantom:medusa:pangolin -->

<!-- cr-indicator-types:potential_issue -->

<!-- This is an auto-generated comment by CodeRabbit -->
