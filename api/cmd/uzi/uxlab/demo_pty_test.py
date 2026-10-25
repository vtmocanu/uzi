"""Stdlib controls for the physical terminal interpreter only."""
import importlib.util
from pathlib import Path
import unittest


spec = importlib.util.spec_from_file_location(
    "demo_pty", Path(__file__).with_name("demo-pty.py"))
demo_pty = importlib.util.module_from_spec(spec)
spec.loader.exec_module(demo_pty)
Screen = demo_pty.Screen


class ScreenTest(unittest.TestCase):
    def setUp(self):
        self.replies = []
        self.screen = Screen(24, 4, self.replies.append)
        self.screen.feed("top\r\nsecond\r\nthird\r\nbottom\x1b[2;3H")

    def assert_rows(self, rows):
        self.assertEqual(self.screen.cells,
                         [list(row.ljust(24)) for row in rows])

    def test_insert_two_lines_shifts_and_truncates(self):
        self.screen.feed("\x1b[2L")
        self.assert_rows(["top", "", "", "second"])
        self.assertEqual((self.screen.x, self.screen.y), (2, 1))
        self.assertEqual(self.replies, [])

    def test_insert_count_is_bounded_by_remaining_rows(self):
        self.screen.feed("\x1b[999999999L")
        self.assert_rows(["top", "", "", ""])
        self.assertEqual((self.screen.x, self.screen.y), (2, 1))

    def test_insert_default_and_zero_mean_one(self):
        for sequence in ("\x1b[L", "\x1b[0L"):
            with self.subTest(sequence=sequence):
                self.setUp()
                self.screen.feed(sequence)
                self.assert_rows(["top", "", "second", "third"])
                self.assertEqual((self.screen.x, self.screen.y), (2, 1))

    def test_insert_at_bottom_preserves_upper_rows(self):
        self.screen.feed("\x1b[4;3H\x1b[2L")
        self.assert_rows(["top", "second", "third", ""])
        self.assertEqual((self.screen.x, self.screen.y), (2, 3))

    def test_private_insert_cursor_and_erase_are_rejected(self):
        for sequence in ("\x1b[?2L", "\x1b[>2L", "\x1b[?2H",
                         "\x1b[>2D", "\x1b[?2J", "\x1b[?2K",
                         "\x1b[?2W"):
            with self.subTest(sequence=sequence):
                self.setUp()
                before = [row[:] for row in self.screen.cells]
                with self.assertRaisesRegex(AssertionError, "unsupported private CSI"):
                    self.screen.feed(sequence)
                self.assertEqual(self.screen.cells, before)
                self.assertEqual((self.screen.x, self.screen.y), (2, 1))
                self.assertEqual(self.replies, [])

    def test_startup_tab_reset_and_legacy_keyboard_no_reply(self):
        screen = Screen(24, 4, self.replies.append)
        screen.feed("\x1b[?5W\x1b[?u\x1b[>1uA\tB\tC")
        self.assertEqual(screen.cells[0], list("A       B       C       "))
        self.assertEqual(screen.cells[1:], [[" "] * 24 for _ in range(3)])
        self.assertEqual((screen.x, screen.y), (17, 0))
        self.assertEqual(self.replies, [])


class RegionTest(unittest.TestCase):
    def setUp(self):
        self.replies = []
        self.screen = Screen(8, 6, self.replies.append)
        self.screen.feed("HEADER\r\nalpha\r\nbravo\r\ncharlie\r\ndelta\r\nFOOTER")

    def assert_grid(self, rows, cursor):
        self.assertEqual(self.screen.cells, [list(row.ljust(8)) for row in rows])
        self.assertEqual((self.screen.x, self.screen.y), cursor)
        self.assertEqual(self.replies, [])

    def test_scroll_down_region_preserves_outside_and_cursor(self):
        for row in (1, 3, 6):
            with self.subTest(row=row):
                self.setUp()
                self.screen.feed(f"\x1b[2;5r\x1b[{row};4H\x1b[2T")
                self.assert_grid(["HEADER", "", "", "alpha", "bravo", "FOOTER"],
                                 (3, row - 1))

    def test_scroll_down_defaults_and_zero_mean_one(self):
        for sequence in ("\x1b[T", "\x1b[0T"):
            with self.subTest(sequence=sequence):
                self.setUp()
                self.screen.feed("\x1b[2;5r\x1b[3;4H" + sequence)
                self.assert_grid(["HEADER", "", "alpha", "bravo", "charlie", "FOOTER"],
                                 (3, 2))

    def test_scroll_down_huge_count_is_bounded(self):
        self.screen.feed("\x1b[2;5r\x1b[3;4H\x1b[999999999T")
        self.assert_grid(["HEADER", "", "", "", "", "FOOTER"], (3, 2))

    def test_scroll_down_default_region(self):
        self.screen.feed("\x1b[2;4H\x1b[2T")
        self.assert_grid(["", "", "HEADER", "alpha", "bravo", "charlie"], (3, 1))

    def test_private_scroll_down_is_rejected(self):
        self.screen.feed("\x1b[2;4H")
        with self.assertRaisesRegex(AssertionError, "unsupported private CSI"):
            self.screen.feed("\x1b[?2T")
        self.assert_grid(["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                         (3, 1))

    def test_delete_lines_preserves_outside_and_cursor(self):
        self.screen.feed("\x1b[2;5r\x1b[3;4H\x1b[2M")
        self.assert_grid(["HEADER", "alpha", "delta", "", "", "FOOTER"], (3, 2))

    def test_delete_lines_huge_count_is_bounded(self):
        self.screen.feed("\x1b[2;5r\x1b[3;4H\x1b[999999999M")
        self.assert_grid(["HEADER", "alpha", "", "", "", "FOOTER"], (3, 2))

    def test_delete_lines_default_and_zero_mean_one(self):
        for sequence in ("\x1b[M", "\x1b[0M"):
            with self.subTest(sequence=sequence):
                self.setUp()
                self.screen.feed("\x1b[2;5r\x1b[3;4H" + sequence)
                self.assert_grid(["HEADER", "alpha", "charlie", "delta", "", "FOOTER"],
                                 (3, 2))

    def test_delete_lines_outside_region_is_ignored(self):
        for row in (1, 6):
            with self.subTest(row=row):
                self.setUp()
                self.screen.feed(f"\x1b[2;5r\x1b[{row};4H\x1b[2M")
                self.assert_grid(
                    ["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                    (3, row - 1))

    def test_delete_lines_default_region(self):
        self.screen.feed("\x1b[3;4H\x1b[2M")
        self.assert_grid(["HEADER", "alpha", "delta", "FOOTER", "", ""], (3, 2))

    def test_private_delete_lines_is_rejected(self):
        self.screen.feed("\x1b[3;4H")
        with self.assertRaisesRegex(AssertionError, "unsupported private CSI"):
            self.screen.feed("\x1b[?2M")
        self.assert_grid(["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                         (3, 2))

    def test_region_homes_cursor_with_origin_mode_off(self):
        self.screen.feed("\x1b[2;5r")
        self.assert_grid(["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                         (0, 0))
        # CUP remains absolute, even with a nonzero top margin.
        self.screen.feed("\x1b[2;4H")
        self.assert_grid(["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                         (3, 1))

    def test_reverse_index_at_top_preserves_header_footer_and_cursor(self):
        self.screen.feed("\x1b[2;5r\x1b[2;4H\x1bM")
        self.assert_grid(["HEADER", "", "alpha", "bravo", "charlie", "FOOTER"],
                         (3, 1))

    def test_reverse_index_away_from_top_moves_only_cursor(self):
        for row, expected_y in ((4, 2), (6, 4), (1, 0)):
            with self.subTest(row=row):
                self.setUp()
                self.screen.feed(f"\x1b[2;5r\x1b[{row};4H\x1bM")
                self.assert_grid(
                    ["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                    (3, expected_y))

    def test_linefeed_at_bottom_preserves_header_footer_and_column(self):
        self.screen.feed("\x1b[2;5r\x1b[5;4H\n")
        self.assert_grid(["HEADER", "bravo", "charlie", "delta", "", "FOOTER"],
                         (3, 4))

    def test_wrap_at_bottom_scrolls_only_region(self):
        self.screen.feed("\x1b[2;5r\x1b[5;8HXY")
        self.assert_grid(["HEADER", "bravo", "charlie", "delta  X", "Y", "FOOTER"],
                         (1, 4))

    def test_linefeed_outside_region_does_not_scroll(self):
        for row, expected_y in ((1, 1), (6, 5)):
            with self.subTest(row=row):
                self.setUp()
                self.screen.feed(f"\x1b[2;5r\x1b[{row};4H\n")
                self.assert_grid(
                    ["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                    (3, expected_y))

    def test_insert_lines_respects_region_and_preserves_cursor(self):
        for count, expected in (
                ("2", ["HEADER", "alpha", "", "", "bravo", "FOOTER"]),
                ("999999999", ["HEADER", "alpha", "", "", "", "FOOTER"])):
            with self.subTest(count=count):
                self.setUp()
                self.screen.feed(f"\x1b[2;5r\x1b[3;4H\x1b[{count}L")
                self.assert_grid(expected, (3, 2))

    def test_insert_outside_region_is_ignored(self):
        for row in (1, 6):
            with self.subTest(row=row):
                self.setUp()
                self.screen.feed(f"\x1b[2;5r\x1b[{row};4H\x1b[2L")
                self.assert_grid(
                    ["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                    (3, row - 1))

    def test_scroll_up_configured_region_preserves_outside_and_cursor(self):
        # SU acts on the entire region, even when the cursor is outside it.
        for row in (1, 3, 6):
            with self.subTest(row=row):
                self.setUp()
                self.screen.feed(f"\x1b[2;5r\x1b[{row};4H\x1b[2S")
                self.assert_grid(["HEADER", "charlie", "delta", "", "", "FOOTER"],
                                 (3, row - 1))

    def test_scroll_up_default_and_zero_mean_one(self):
        for sequence in ("\x1b[S", "\x1b[0S"):
            with self.subTest(sequence=sequence):
                self.setUp()
                self.screen.feed("\x1b[2;5r\x1b[3;4H" + sequence)
                self.assert_grid(["HEADER", "bravo", "charlie", "delta", "", "FOOTER"],
                                 (3, 2))

    def test_scroll_up_huge_count_is_bounded_by_region_height(self):
        self.screen.feed("\x1b[2;5r\x1b[3;4H\x1b[999999999S")
        self.assert_grid(["HEADER", "", "", "", "", "FOOTER"], (3, 2))

    def test_scroll_up_default_region_is_full_screen(self):
        for count, expected in (
                ("2", ["bravo", "charlie", "delta", "FOOTER", "", ""]),
                ("", ["alpha", "bravo", "charlie", "delta", "FOOTER", ""]),
                ("0", ["alpha", "bravo", "charlie", "delta", "FOOTER", ""]),
                ("999999999", ["", "", "", "", "", ""])):
            with self.subTest(count=count):
                self.setUp()
                self.screen.feed(f"\x1b[3;4H\x1b[{count}S")
                self.assert_grid(expected, (3, 2))

    def test_private_scroll_up_is_rejected_without_changes(self):
        for sequence in ("\x1b[?2S", "\x1b[>2S"):
            with self.subTest(sequence=sequence):
                self.setUp()
                self.screen.feed("\x1b[2;5r\x1b[3;4H")
                with self.assertRaisesRegex(AssertionError, "unsupported private CSI"):
                    self.screen.feed(sequence)
                self.assert_grid(
                    ["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                    (3, 2))

    def test_default_and_reset_regions_scroll_full_screen(self):
        for reset in (None, "\x1b[r", "\x1b[;r", "\x1b[0;0r",
                      "\x1b[1r", "\x1b[;6r"):
            with self.subTest(reset=reset):
                self.setUp()
                if reset is not None:
                    self.screen.feed("\x1b[2;5r" + reset)
                    self.assertEqual((self.screen.x, self.screen.y), (0, 0))
                self.screen.feed("\x1b[1;4H\x1bM")
                self.assert_grid(["", "HEADER", "alpha", "bravo", "charlie", "delta"],
                                 (3, 0))

    def test_each_margin_can_default_independently(self):
        for sequence in ("\x1b[2r", "\x1b[2;r", "\x1b[2;0r"):
            with self.subTest(sequence=sequence):
                self.setUp()
                self.screen.feed(sequence + "\x1b[2;4H\x1bM")
                self.assert_grid(["HEADER", "", "alpha", "bravo", "charlie", "delta"],
                                 (3, 1))

    def test_invalid_or_private_regions_are_rejected_without_changes(self):
        for sequence in ("\x1b[5;2r", "\x1b[2;2r", "\x1b[1;7r",
                         "\x1b[7r", "\x1b[999999999;6r",
                         "\x1b[1;6;2r", "\x1b[?2;5r", "\x1b[>2;5r"):
            with self.subTest(sequence=sequence):
                self.setUp()
                self.screen.feed("\x1b[2;5r\x1b[2;4H")
                with self.assertRaises(AssertionError):
                    self.screen.feed(sequence)
                self.assert_grid(
                    ["HEADER", "alpha", "bravo", "charlie", "delta", "FOOTER"],
                    (3, 1))
                # A rejected sequence remains pending; discard it to inspect
                # the previously configured region through a display operation.
                self.screen.pending = ""
                self.screen.feed("\x1bM")
                self.assert_grid(["HEADER", "", "alpha", "bravo", "charlie", "FOOTER"],
                                 (3, 1))

    def test_resize_resets_margins_to_full_new_height(self):
        for rows in (4, 6, 8):
            with self.subTest(rows=rows):
                self.setUp()
                self.screen.feed("\x1b[2;3r\x1b[3;4H")
                self.screen.resize(8, rows)
                self.screen.feed(f"\x1b[{rows};4H\n")
                expected = {
                    4: ["alpha", "bravo", "charlie", ""],
                    6: ["alpha", "bravo", "charlie", "delta", "FOOTER", ""],
                    8: ["alpha", "bravo", "charlie", "delta", "FOOTER", "", "", ""],
                }[rows]
                self.assert_grid(expected, (3, rows - 1))
                self.screen.feed("\x1b[1;4H\x1bM")
                self.assert_grid([""] + expected[:-1], (3, 0))


class CharacterDeleteTest(unittest.TestCase):
    def setUp(self):
        self.replies = []
        self.screen = Screen(8, 3, self.replies.append)
        self.screen.feed("HEADER\r\nabcdefgh\r\nFOOTER\x1b[2;3H")

    def assert_row(self, row, cursor=(2, 1)):
        self.assertEqual(self.screen.cells,
                         [list("HEADER  "), list(row), list("FOOTER  ")])
        self.assertEqual((self.screen.x, self.screen.y), cursor)
        self.assertEqual(self.replies, [])

    def test_delete_closes_gap_and_blanks_right_edge(self):
        self.screen.feed("\x1b[3P")
        self.assert_row("abfgh   ")

    def test_delete_default_and_zero_mean_one(self):
        for sequence in ("\x1b[P", "\x1b[0P"):
            with self.subTest(sequence=sequence):
                self.setUp()
                self.screen.feed(sequence)
                self.assert_row("abdefgh ")

    def test_delete_count_is_bounded_by_remaining_columns(self):
        self.screen.feed("\x1b[999999999P")
        self.assert_row("ab      ")

    def test_delete_at_last_column_preserves_cursor(self):
        self.screen.feed("\x1b[2;8H\x1b[16P")
        self.assert_row("abcdefg ", (7, 1))

    def test_delete_is_not_limited_by_vertical_scroll_region(self):
        self.screen.feed("\x1b[2;3r\x1b[1;3H\x1b[3P")
        self.assertEqual(self.screen.cells,
                         [list("HER     "), list("abcdefgh"), list("FOOTER  ")])
        self.assertEqual((self.screen.x, self.screen.y), (2, 0))

    def test_private_delete_is_rejected(self):
        with self.assertRaisesRegex(AssertionError, "unsupported private CSI"):
            self.screen.feed("\x1b[?3P")
        self.assert_row("abcdefgh")


if __name__ == "__main__":
    unittest.main()
