// Lightweight, zxcvbn-style password strength heuristic. The authoritative
// minimum (12 chars) is enforced server-side; this is UX feedback only, kept
// dependency-free for a small bundle.

export interface Strength {
  score: 0 | 1 | 2 | 3 | 4;
  label: string;
}

export function scorePassword(pw: string): Strength {
  let score = 0;
  if (pw.length >= 12) score++;
  if (pw.length >= 16) score++;
  if (/[a-z]/.test(pw) && /[A-Z]/.test(pw)) score++;
  if (/\d/.test(pw)) score++;
  if (/[^A-Za-z0-9]/.test(pw)) score++;

  // Penalize obvious patterns.
  if (/^(.)\1+$/.test(pw) || /^(0123|1234|abcd|qwerty|password)/i.test(pw)) {
    score = Math.min(score, 1);
  }

  const clamped = Math.min(score, 4) as Strength["score"];
  const labels = ["very weak", "weak", "fair", "good", "strong"];
  return { score: clamped, label: labels[clamped] };
}
