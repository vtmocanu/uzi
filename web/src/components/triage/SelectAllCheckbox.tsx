import { useEffect, useRef } from "react";

// SelectAllCheckbox is the list-header "select all" control the Judge page and the Findings
// page share (PRD #1183). The tri-state VISUAL — checked / unchecked / indeterminate ("some
// but not all shown are selected") — is not expressible through a React `checked` prop, so the
// indeterminate flag is set imperatively on the DOM node via a ref, the only way to drive that
// element property.
//
// `label` is UI copy (e.g. "Select all N shown"), not model-authored text, so it is used as
// the accessible name verbatim; a model value would have to be stripUnsafeChars'd before it
// reached this aria-label (.claude/rules/web.md), but none does.
export function SelectAllCheckbox({
  checked,
  indeterminate = false,
  onChange,
  label,
}: {
  checked: boolean;
  indeterminate?: boolean;
  onChange: (v: boolean) => void;
  label: string;
}) {
  const ref = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (ref.current) ref.current.indeterminate = indeterminate;
  }, [indeterminate, checked]);

  return (
    <label className="inline-flex items-center gap-2 text-xs text-muted">
      <input
        ref={ref}
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        aria-label={label}
        className="h-4 w-4 shrink-0 accent-brand"
      />
      <span>{label}</span>
    </label>
  );
}
