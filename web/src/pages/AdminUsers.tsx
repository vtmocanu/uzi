import { useCallback, useEffect, useState } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError, type User } from "../lib/api";
import { Alert, Badge, Button, Card, ListSkeleton, PageHeader } from "../components/ui";

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
      <PageHeader
        title="Users"
        description="Deactivating a user blocks their login and immediately ends every active session."
      />
      {error && <Alert message={error} />}
      {loading ? (
        <ListSkeleton rows={4} />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">Email</th>
                  <th className="px-4 py-3 font-medium">Name</th>
                  <th className="px-4 py-3 font-medium">Role</th>
                  <th className="px-4 py-3 font-medium">Status</th>
                  <th className="px-4 py-3 font-medium">Last login</th>
                  <th className="px-4 py-3 text-right font-medium">Action</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {users.map((u) => (
                  <tr key={u.id} className="transition-colors hover:bg-raised/30">
                    <td className="px-4 py-3 text-fg">{u.email}</td>
                    <td className="px-4 py-3 text-muted">{u.display_name ?? "—"}</td>
                    <td className="px-4 py-3">
                      {u.is_admin ? <Badge tone="brand">Admin</Badge> : <Badge>User</Badge>}
                    </td>
                    <td className="px-4 py-3">
                      <Badge tone={u.is_active ? "ok" : "danger"} dot>
                        {u.is_active ? "Active" : "Deactivated"}
                      </Badge>
                    </td>
                    <td className="px-4 py-3 text-muted">
                      {u.last_login ? new Date(u.last_login).toLocaleString() : "—"}
                    </td>
                    <td className="px-4 py-3 text-right">
                      {u.id === me?.id ? (
                        <span className="text-xs text-faint">you</span>
                      ) : (
                        <Button
                          variant={u.is_active ? "danger" : "secondary"}
                          size="sm"
                          disabled={busyId === u.id}
                          onClick={() => toggle(u)}
                        >
                          {u.is_active ? "Deactivate" : "Activate"}
                        </Button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </div>
  );
}
