import { Link } from "react-router-dom";
import { MOCK_MODE } from "../lib/api";
import { Button, Card } from "../components/ui";
import { FactoryIcon } from "../components/icons";

export function Landing() {
  return (
    <div className="mx-auto max-w-2xl pt-8 text-center">
      <div className="mx-auto flex h-14 w-14 items-center justify-center rounded-2xl bg-brand/15 text-3xl text-brand">
        <FactoryIcon />
      </div>
      <h1 className="mt-5 text-4xl font-bold tracking-tight">Uzinele Întunecate</h1>
      <p className="mt-3 text-lg text-muted">
        An AI dark factory: agents pick up your PRD-labeled issues, plan, wait for your approval,
        then implement and open the merge/pull request. <span className="text-fg">Never touching main.</span>
      </p>
      {MOCK_MODE && (
        <p className="mt-3 text-sm text-brand">
          This is a live demo running entirely in your browser — any credentials sign you in.
        </p>
      )}
      <Card className="mt-8 text-left">
        <p className="text-sm text-muted">
          The first account created becomes the administrator. Everyone after that is a regular
          user until an admin says otherwise.
        </p>
        <div className="mt-6 flex justify-center gap-3">
          <Link to="/register">
            <Button>Create an account</Button>
          </Link>
          <Link to="/login">
            <Button variant="secondary">Log in</Button>
          </Link>
        </div>
      </Card>
    </div>
  );
}
