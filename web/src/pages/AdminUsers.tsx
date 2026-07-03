import { useCallback, useEffect, useState } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type User } from "../lib/api";
import { Alert, Button, Card } from "../components/ui";

export function AdminUsers() {
  const { user: me } = useAuth();
  const [users, setUsers] = useState<User[]>([]);
  const [error, setError] = useState("");
  const [busyId, setBusyId] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    try {
      const { users } = await api.listUsers();
      setUsers(users);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Failed to load users");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const toggle = async (u: User) => {
    setError("");
    setBusyId(u.id);
    try {
      const { user } = await api.setUserActive(u.id, !u.is_active);
      setUsers((prev) => prev.map((x) => (x.id === user.id ? user : x)));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Update failed");
    } finally {
      setBusyId(null);
    }
  };

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Users</h1>
        <p className="mt-1 text-slate-400">
          Deactivating a user blocks their login and immediately ends every active session.
        </p>
      </div>
      {error && <Alert message={error} />}
      <Card className="p-0">
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="border-b border-slate-800 text-slate-400">
              <tr>
                <th className="px-4 py-3 font-medium">Email</th>
                <th className="px-4 py-3 font-medium">Name</th>
                <th className="px-4 py-3 font-medium">Role</th>
                <th className="px-4 py-3 font-medium">Status</th>
                <th className="px-4 py-3 font-medium">Last login</th>
                <th className="px-4 py-3 font-medium text-right">Action</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-800">
              {loading ? (
                <tr>
                  <td colSpan={6} className="px-4 py-6 text-center text-slate-500">
                    Loading…
                  </td>
                </tr>
              ) : (
                users.map((u) => (
                  <tr key={u.id}>
                    <td className="px-4 py-3">{u.email}</td>
                    <td className="px-4 py-3 text-slate-400">{u.display_name ?? "—"}</td>
                    <td className="px-4 py-3">{u.is_admin ? "Admin" : "User"}</td>
                    <td className="px-4 py-3">
                      <span
                        className={
                          u.is_active ? "text-emerald-400" : "text-rose-400"
                        }
                      >
                        {u.is_active ? "Active" : "Deactivated"}
                      </span>
                    </td>
                    <td className="px-4 py-3 text-slate-400">
                      {u.last_login ? new Date(u.last_login).toLocaleString() : "—"}
                    </td>
                    <td className="px-4 py-3 text-right">
                      {u.id === me?.id ? (
                        <span className="text-xs text-slate-600">you</span>
                      ) : (
                        <Button
                          variant={u.is_active ? "danger" : "ghost"}
                          disabled={busyId === u.id}
                          onClick={() => toggle(u)}
                        >
                          {u.is_active ? "Deactivate" : "Activate"}
                        </Button>
                      )}
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
}
