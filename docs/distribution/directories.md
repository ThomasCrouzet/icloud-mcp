# MCP directory submissions

No submission was made for this list. Read the current rules again before each
submission. Prices and forms change.

Checked on 2026-10-31.

| Directory | Submission path | Required fields | Current status |
|-----------|-----------------|-----------------|----------------|
| Official MCP Registry | `mcp-publisher publish` with `server.json` | `name`, `version`, `description` (100 characters or fewer), `repository.url`, `websiteUrl` | Active at `0.4.0`. Publish `0.4.2` after the archive and signature check. |
| Glama | Automatic GitHub index. Use "Claim" on the listing. | Public GitHub repository. Claim uses GitHub authentication. | Indexed. `https://glama.ai/mcp/servers/@ThomasCrouzet/icloud-mcp` returned HTTP 200 and shows the project. |
| PulseMCP | Web form at `/submit` | Name, repository, description, remote flag (when the form reopens) | Not listed. Submissions and listing changes are paused. The page says "Last updated: September 3, 2026". |
| mcp.so | Web form at `/submit` | Repository URL, name, listing type. Paid placement is optional. | Not listed. The site search returned 0 results for `icloud-mcp`. |
| mcpservers.org | Web form at `/submit` | Name, category, short description, repository or website or documentation URL, optional official registry name, remote flag, contact email | Unverified. Automated checks returned HTTP 403. No submission was made. |
| Cline MCP Marketplace | GitHub issue form in `cline/mcp-marketplace` | Repository URL, 400x400 PNG logo, reason for addition, proof that Cline completed setup from the README | Not listed. GitHub issue search found no exact submission. |
| MCP Market | Web form at `/submit` | GitHub repository URL, optional remote flag | Not listed. The direct server URL returned HTTP 404. |
| Awesome MCP Servers | Pull request to `punkpeye/awesome-mcp-servers` | Name linked to the repository, brief description, category, alphabetical order | Not listed. A README search on 2026-10-31 found no `ThomasCrouzet/icloud-mcp` entry. |
| Docker MCP Registry | Pull request to `docker/mcp-registry` | `server.yaml`, category, Dockerfile or public remote endpoint, environment variables and secrets, icon | Not eligible. The project has no Dockerfile or public remote endpoint. Docker is only a build tool for one release target. |
| Smithery | `smithery.ai/new` or the Smithery CLI | Public HTTPS MCP URL, or an MCPB bundle for a local stdio server | Not eligible. The project ships a local stdio binary and has no MCPB bundle. The direct server URL returned HTTP 404. |

## Notes

- Do not submit to PulseMCP while submissions are paused.
- Do not submit to Docker or Smithery until the project has a container image,
  a public remote endpoint, or an MCPB bundle.
- mcp.so and mcpservers.org offer paid placement. MCP Market offers a paid fast
  queue. Use a free path unless the maintainer approves a payment.
- Glama indexes the repository without a submission. Use the claim flow to
  control the listing.
- Record each submission date, account, and public URL outside this repository.
  Keep confirmation mail and reports in local storage.
