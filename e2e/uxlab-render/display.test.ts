import assert from "node:assert/strict";
import { test } from "node:test";
import { displayJSON } from "./display.js";

test("display JSON escapes every bidi control while preserving raw fields", () => {
  const raw = "\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069";
  const data = { stdout: raw + "ruby 4.0.6", nested: [raw], ordinary: "café" };
  const output = displayJSON(data);
  assert.equal(output, '{"stdout":"\\u061c\\u200e\\u200f\\u202a\\u202b\\u202c\\u202d\\u202e\\u2066\\u2067\\u2068\\u2069ruby 4.0.6","nested":["\\u061c\\u200e\\u200f\\u202a\\u202b\\u202c\\u202d\\u202e\\u2066\\u2067\\u2068\\u2069"],"ordinary":"café"}');
  assert.deepEqual(JSON.parse(output), data);
  assert.equal(data.stdout, raw + "ruby 4.0.6");
});
