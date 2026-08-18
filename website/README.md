# Public website

A single, self-contained static landing/docs page for llm-gateway
(`index.html`, no build step, no external dependencies). It reuses the in-app
Docs content and the admin panel's visual language.

## Preview
```bash
python -m http.server 8080 --directory website
# open http://127.0.0.1:8080
```

## Publish (deferred — needs a domain/host decision)
Any static host works: GitHub Pages, Cloudflare Pages, S3+CloudFront, or a Caddy
`file_server` on the same box as the gateway. Before publishing:

- **Where** it lives (e.g. a `*.example.com` subdomain per the portfolio DNS
  convention, GitHub Pages, or the repo's landing). Confirm the host + domain.
- Set the **GitHub link** (currently a placeholder `https://github.com/`) to the
  real repository URL.
- If hosting the marketing page and the gateway on the same domain, keep them on
  distinct paths/subdomains (the gateway's `/admin` is an operator surface).

Publishing is intentionally left as an operator step rather than pushed to a live
domain automatically.
