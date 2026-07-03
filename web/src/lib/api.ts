// Thin API client. All requests are same-origin (nginx proxies /api) and rely
// on the HttpOnly auth cookie, so we always send credentials. State-changing
// requests echo the readable CSRF cookie back in the X-CSRF-Token header.

export interface User {
  id: string;
  email: string;
  display_name: string | null;
  is_admin: boolean;
  is_active: boolean;
  created_at: string;
  last_login: string | null;
}

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

function readCookie(name: string): string | null {
  const match = document.cookie.match(new RegExp("(?:^|; )" + name + "=([^;]*)"));
  return match ? decodeURIComponent(match[1]) : null;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (method !== "GET" && method !== "HEAD") {
    const csrf = readCookie("uzi_csrf");
    if (csrf) headers["X-CSRF-Token"] = csrf;
  }

  const res = await fetch(`/api${path}`, {
    method,
    headers,
    credentials: "same-origin",
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  let payload: unknown = null;
  const text = await res.text();
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = null;
    }
  }

  if (!res.ok) {
    const message =
      (payload as { error?: string } | null)?.error ?? `request failed (${res.status})`;
    throw new ApiError(res.status, message);
  }
  return payload as T;
}

export const api = {
  register: (email: string, password: string, displayName: string) =>
    request<{ user: User }>("POST", "/auth/register", {
      email,
      password,
      display_name: displayName,
    }),
  login: (email: string, password: string) =>
    request<{ user: User }>("POST", "/auth/login", { email, password }),
  logout: () => request<{ status: string }>("POST", "/auth/logout"),
  me: () => request<{ user: User }>("GET", "/auth/me"),
  listUsers: () => request<{ users: User[] }>("GET", "/admin/users"),
  setUserActive: (id: string, isActive: boolean) =>
    request<{ user: User }>("PATCH", `/admin/users/${id}`, { is_active: isActive }),
};
