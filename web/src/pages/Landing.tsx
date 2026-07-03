import { Link } from "react-router-dom";
import { Button, Card } from "../components/ui";

export function Landing() {
  return (
    <div className="mx-auto max-w-2xl text-center">
      <h1 className="text-4xl font-bold tracking-tight">Uzinele Întunecate</h1>
      <p className="mt-3 text-lg text-slate-400">
        An AI dark factory. This is the front door — register an account to get in.
      </p>
      <Card className="mt-8 text-left">
        <p className="text-sm text-slate-400">
          The first account created becomes the administrator. Everyone after that is a regular
          user until an admin says otherwise.
        </p>
        <div className="mt-6 flex justify-center gap-3">
          <Link to="/register">
            <Button>Create an account</Button>
          </Link>
          <Link to="/login">
            <Button variant="ghost">Log in</Button>
          </Link>
        </div>
      </Card>
    </div>
  );
}
