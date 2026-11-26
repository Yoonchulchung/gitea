# Security events

The **Security events** admin page (`/-/admin/company-security`) answers "were we attacked": what was tried against the platform in the last 24 hours, 7 days or 30 days, where it came from, and what it aimed at. Every event on it was refused or failed; the page records that it was tried.

| Event | When |
|---|---|
| Failed sign-in | A sign-in or two-factor code to Gitea was wrong |
| Visitor blocked | The proxy guard cut a visitor off after repeated violations |
| Scanner program | A request came from a known attack tool |
| Probe path | A request asked for something only an attacker looks for, such as `/.env` or `/wp-admin` |
| Over request limit | A visitor sent requests faster than the guard allows |
| Request from another site | Another site tried to submit a form to an app behind sign-in |
| Opened without permission | Someone outside an app's project tried to open a members-only app |
| Request too large | A request body was over the size limit |
| Outbound refused | An app tried to reach a destination it is not allowed to |
| Download blocked | An app tried to send a file it is not allowed to |

The page says an attack was attempted when a visitor was blocked, a scanner or probe was seen, an app was refused an outbound destination, or there were 10 or more failed sign-ins.

## Email alert

Add a mail rule for **Possible attack detected** on the mail page. Administrators then get an email when, within 10 minutes, there are 30 or more events, 10 or more failed sign-ins, or a visitor is blocked. At most one email is sent an hour.

## What it does not cover

Only what reaches Gitea. Attacks on the server itself, such as SSH logins or other ports, are in the server's own logs.
