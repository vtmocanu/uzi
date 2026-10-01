---
title: Connecting a product
order: 115
audience: user
---

# Connecting a product

An external product that an admin registered as an [OAuth client](./oauth-clients.md) can ask for access to your uzi account without you copying a token. You approve on a uzi page, and the product then works for you until you disconnect it. This is the alternative to pasting a [product token](./product-tokens.md).

## Approve a connection

1. In the product, choose to connect uzi. Your browser opens uzi's **Connect** page. If you are signed out, sign in first (password or single sign-on) and you return to the same page.
2. Read what the page shows. It is always shown, never skipped:
   - the product's **name and description**, written by your admin and shown as plain text;
   - the **site** you are sent back to when you decide;
   - what the connection can do: **Run jobs** (`jobs:run`) and **Read jobs and their results** (`jobs:read`);
   - this notice: the product may run jobs automatically on your behalf, as you, using your model credential configured in uzi.
3. Choose **Approve** or **Deny**. Either way you are sent back to the product. Open the link in the same browser you started in: another browser cannot approve it, and an unused request expires after 5 minutes.

## What a connection can do

- It works only on [`/api/v1`](./product-tokens.md#what-it-can-and-cannot-reach), exactly like a product token with the approved scopes, and never with admin authority. Jobs it starts run as you and on your credential ([Jobs](./jobs.md)).
- The product holds an **access token** that lasts one hour and a **refresh token**. A connection has at most 10 live access tokens at a time.
- Approving again replaces the scopes you approved before. If the new scopes drop one the earlier tokens held, those tokens stop working; reconnecting with the same or more scopes does not cancel running jobs.

## Disconnect

A connection stops working on its **next request** when any of these happens, and every access token, the refresh token and any approved but unused code of that connection go with it:

| Action | Who |
|---|---|
| **Revoke all** in **Settings → Access**. Its button counts your live connections (not tokens), so it is offered even when the only thing live is a connection. | you |
| Revoke **one** of its access tokens by id (through the same call as any product token) | you, or an admin on **Admin → Products** |
| The product, if it asks uzi to revoke its refresh token | the product |
| The product is disabled or deleted, or your account is deactivated | an admin |

Revoking cancels the connection's non-terminal jobs, including jobs started by an access token that had already expired. A password change and logging out do not disconnect a product, as for any token. A connection's tokens are not listed with your own product tokens and do not count towards the 10-token limit on those.
