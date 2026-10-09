import { expect, it } from "vitest";
import { createSlugger } from "./headingSlug";

// Selected literal cases from github-slugger's ISC-licensed fixtures:
// https://github.com/Flet/github-slugger/blob/master/test/fixtures.json
// Copyright (c) 2015, Dan Flettre <fletd01@yahoo.com>
// Permission to use, copy, modify, and/or distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION
// OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN
// CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
// Sample data only; no copied code or full github-slugger parity claim.
it.each([
  ["bravoCharlieDelta", "bravocharliedelta"],
  ["__proto__", "__proto__"],
  ["heading with a - dash", "heading-with-a---dash"],
  ["heading with an _ underscore", "heading-with-an-_-underscore"],
  ["heading with a period.txt", "heading-with-a-periodtxt"],
  ["exchange.bind_headers(exchange, routing [, bindCallback])", "exchangebind_headersexchange-routing--bindcallback"],
  [" a ", "-a-"],
  ["The forge screens: `pulls` and `ci`", "the-forge-screens-pulls-and-ci"],
  ["🔴 The rule", "-the-rule"],
  ["A & B", "a--b"],
  ["Été e\u0301 中文 ١²Ⅳ foo\u203Fbar", "été-e\u0301-中文-١²ⅳ-foo\u203Fbar"],
  ["a\tb\nc\u00A0d", "abcd"],
  ["", ""],
])("slugs %j as %j", (text, id) => {
  expect(createSlugger().slug(text)).toBe(id);
});

it.each([
  [["x", "x", "x"], ["x", "x-1", "x-2"]],
  [["x", "x", "x-1"], ["x", "x-1", "x-1-1"]],
  [["x-1", "x", "x", "x-1"], ["x-1", "x", "x-2", "x-1-1"]],
  [["", "", "-1"], ["", "-1", "-1-1"]],
])("avoids every emitted ID for %j", (texts, ids) => {
  const slugger = createSlugger();
  expect(texts.map((text) => slugger.slug(text))).toEqual(ids);
});
