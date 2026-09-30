// JOB_TYPES is the web mirror of the known job types (PRD #1908): runkind.JobTypes() in
// api/internal/runkind/runkind.go, which mirrors the runs_job_type_check and
// products_allowed_job_types_check CHECK constraints. Production web cannot read the Go
// source at runtime, so the list is hard-coded here and jobTypes.test.ts pins it to the Go
// declaration, in order. Adding a job type server-side reddens that test until it lands
// here too, so the admin allow-list never silently lacks a checkbox.
export const JOB_TYPES = ["research"] as const;

// jobTypeLabel names a job type for people. An unknown value (a newer server) degrades to
// the raw type with underscores spaced, the way runKindLabel does.
export function jobTypeLabel(type: string): string {
  switch (type) {
    case "research":
      return "Research";
    default:
      return type.replace(/_/g, " ");
  }
}
