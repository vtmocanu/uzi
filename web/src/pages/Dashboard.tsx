import { useAuth } from "../auth/AuthContext";
import { Card } from "../components/ui";

export function Dashboard() {
  const { user } = useAuth();
  if (!user) return null;

  const rows: [string, string][] = [
    ["Email", user.email],
    ["Display name", user.display_name ?? "—"],
    ["Role", user.is_admin ? "Administrator" : "User"],
    ["Account status", user.is_active ? "Active" : "Deactivated"],
    ["Joined", new Date(user.created_at).toLocaleString()],
    ["Last login", user.last_login ? new Date(user.last_login).toLocaleString() : "—"],
  ];

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">
          Welcome{user.display_name ? `, ${user.display_name}` : ""}
        </h1>
        <p className="mt-1 text-slate-400">
          This is the factory dashboard. Job submission and agent control land here in later
          milestones.
        </p>
      </div>
      <Card>
        <h2 className="text-sm font-semibold uppercase tracking-wide text-slate-500">
          Your account
        </h2>
        <dl className="mt-4 divide-y divide-slate-800">
          {rows.map(([k, v]) => (
            <div key={k} className="flex justify-between py-2 text-sm">
              <dt className="text-slate-400">{k}</dt>
              <dd className="text-slate-100">{v}</dd>
            </div>
          ))}
        </dl>
      </Card>
    </div>
  );
}
