package oauth

import "html/template"

// consentTmpl renders the authorization consent screen. The user pastes their
// PushWard API key (hlk_…) once; it is validated upstream and then stored
// encrypted server-side. All OAuth parameters are carried as hidden fields and
// a CSRF token guards the POST.
var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize PushWard MCP</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: -apple-system, system-ui, sans-serif; max-width: 28rem; margin: 3rem auto; padding: 0 1rem; line-height: 1.5; }
  .card { border: 1px solid color-mix(in srgb, currentColor 18%, transparent); border-radius: 14px; padding: 1.5rem; }
  h1 { font-size: 1.25rem; margin: 0 0 .25rem; }
  .sub { opacity: .7; font-size: .9rem; margin: 0 0 1.25rem; }
  label { display: block; font-weight: 600; margin: 1rem 0 .35rem; font-size: .9rem; }
  input[type=password] { width: 100%; box-sizing: border-box; padding: .7rem .8rem; border-radius: 10px; border: 1px solid color-mix(in srgb, currentColor 25%, transparent); font: inherit; }
  button { margin-top: 1.25rem; width: 100%; padding: .75rem; border: 0; border-radius: 10px; background: #2c6ef2; color: #fff; font: inherit; font-weight: 600; cursor: pointer; }
  .err { color: #d33; font-size: .9rem; margin-top: .75rem; }
  .scope { font-size: .85rem; opacity: .75; margin-top: 1rem; }
  .warn { font-size: .85rem; color: #b76b00; margin: .5rem 0 0; }
  .dest { font-size: .9rem; margin-top: 1rem; padding: .6rem .7rem; border-radius: 8px; background: color-mix(in srgb, currentColor 8%, transparent); }
  code { background: color-mix(in srgb, currentColor 12%, transparent); padding: .1rem .3rem; border-radius: 5px; }
</style>
</head>
<body>
  <div class="card">
    <h1>Authorize access</h1>
{{- if .Verified}}
    <p class="sub"><strong>{{.ClientName}}</strong> is requesting access to your PushWard account.</p>
{{- else}}
    <p class="sub">An app identifying itself as <strong>{{.ClientName}}</strong> is requesting access to your PushWard account.</p>
    <p class="warn">⚠ This name is not verified. Only continue if you recognize the destination below.</p>
{{- end}}
    {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
    <form method="post" action="/oauth/authorize">
      <label for="api_key">PushWard API key</label>
      <input id="api_key" name="api_key" type="password" placeholder="hlk_…" autocomplete="off" autocapitalize="off" spellcheck="false" required>
      <input type="hidden" name="csrf" value="{{.CSRF}}">
      <input type="hidden" name="response_type" value="{{.ResponseType}}">
      <input type="hidden" name="client_id" value="{{.ClientID}}">
      <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
      <input type="hidden" name="state" value="{{.State}}">
      <input type="hidden" name="scope" value="{{.Scope}}">
      <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
      <input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
      <input type="hidden" name="resource" value="{{.Resource}}">
      <button type="submit">Authorize</button>
      <p class="dest">After you approve, your authorization will be sent to <code>{{.RedirectHost}}</code>. Make sure you trust it.</p>
    </form>
  </div>
</body>
</html>`))

type consentData struct {
	ClientName          string
	Verified            bool // CIMD (origin-verified) client vs anonymous DCR
	ResponseType        string
	ClientID            string
	RedirectURI         string
	RedirectHost        string
	State               string
	Scope               string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	CSRF                string
	Error               string
}
