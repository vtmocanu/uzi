// ScopePicker — the admin-only CLI-token scope selector, shared by the Settings
// mint form (CliTokens) and the browser-login consent page (CliAuth). Extracted
// so the security property "the scope control is hidden from non-admins" lives in
// ONE place: it renders nothing unless `admin`, so the two call sites can't drift
// into disagreeing. The server is the real boundary (it 403s an admin_ro mint by
// a non-admin); this is UX. Kept a <Select> (accessible; labelled via Field).

import { type CliTokenScope } from "../lib/api";
import { Field, Select } from "./ui";

export function ScopePicker({
  admin,
  value,
  onChange,
  id = "cli-token-scope",
  className,
}: {
  admin: boolean;
  value: CliTokenScope;
  onChange: (scope: CliTokenScope) => void;
  id?: string;
  className?: string;
}) {
  // The single gate. Non-admins never render the control at all.
  if (!admin) return null;
  return (
    <div className={className}>
      <Field label="Scope" htmlFor={id}>
        <Select id={id} value={value} onChange={(e) => onChange(e.target.value as CliTokenScope)}>
          <option value="user">User — your own access</option>
          <option value="admin_ro">Admin (read-only) — whole factory</option>
        </Select>
      </Field>
    </div>
  );
}
