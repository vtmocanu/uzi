// Lazily imported by Page.tsx, so it builds into its own content-hashed chunk. The build
// tag is part of the chunk, so v1 and v2 builds get different file names: a rollout that
// replaces the files makes an open document's dynamic import of the old name 404.
export default function SubView() {
  return <p data-testid="example-subview">Sub view, build {__MODULE_BUILD_TAG__}</p>;
}
