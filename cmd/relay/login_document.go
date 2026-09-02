package main

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
)

// loginNoncePlaceholder is replaced with a fresh value on every render, in
// both the header and the two tags it authorises.
const loginNoncePlaceholder = "__RELAY_CSP_NONCE__"

// serveDocument renders the login page under a strict Content-Security-Policy.
// The page loads nothing: no CDN, no framework, no external font, no image.
// `default-src 'none'` is what makes that a rule rather than a habit, and
// connect-src 'self' is the one thing it must allow — the page's own fetches
// back to the two POST routes.
func (lr *loginRoutes) serveDocument(w http.ResponseWriter, r *http.Request) {
	nonce, err := loginNonce()
	if err != nil {
		slog.Error("login: could not generate a CSP nonce", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Security-Policy", strings.Join([]string{
		"default-src 'none'",
		"script-src 'nonce-" + nonce + "'",
		"style-src 'nonce-" + nonce + "'",
		"connect-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; "))
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(strings.ReplaceAll(loginDocument, loginNoncePlaceholder, nonce))); err != nil {
		slog.Warn("login: could not write the login document", "error", err)
	}
}

func loginNonce() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(raw), nil
}

// The document is small and hand-written on purpose: it is security-relevant
// code a reviewer must be able to read end to end, and every parameter
// ADR-016 decision 6 pins — ES256 alone, userVerification required,
// residentKey discouraged, allowCredentials on every assertion — is visible
// in one screen of JavaScript rather than assembled by a library.
//
// This is subtle: the minted token is held in a closure and deliberately
// never written to localStorage, a cookie, or the DOM. A reload runs the
// ceremony again, which is the cost ADR-016 decision 3 chooses over
// persistence.
const loginDocument = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>relay login</title>
<style nonce="__RELAY_CSP_NONCE__">
  :root { color-scheme: light dark; }
  body { font: 15px/1.5 system-ui, sans-serif; margin: 0; padding: 3rem 1.5rem; }
  main { max-width: 26rem; margin: 0 auto; }
  h1 { font-size: 1.4rem; margin: 0 0 .25rem; }
  p.sub { margin: 0 0 2rem; opacity: .7; }
  label { display: block; font-weight: 600; margin-bottom: .35rem; }
  input { width: 100%; box-sizing: border-box; padding: .55rem .6rem; font: inherit;
          font-family: ui-monospace, monospace; letter-spacing: .04em; }
  .row { display: flex; gap: .6rem; margin-top: 1rem; }
  button { flex: 1; padding: .6rem; font: inherit; cursor: pointer; }
  #status { margin-top: 1.5rem; min-height: 3rem; white-space: pre-wrap; }
  .bad { color: #b00020; }
  footer { margin-top: 2.5rem; font-size: .85rem; opacity: .7; }
</style>
</head>
<body>
<main>
  <h1>relay</h1>
  <p class="sub">Sign in with a passkey, or register one with a code from
    <code>relay login enrol</code>.</p>

  <label for="code">Login code</label>
  <input id="code" name="code" autocomplete="off" spellcheck="false" placeholder="only needed to register">

  <div class="row">
    <button id="register" type="button">Register a passkey</button>
    <button id="signin" type="button">Sign in</button>
  </div>

  <p id="status" role="status"></p>

  <footer>The credential this page receives is held in memory only. Reloading
    signs you out.</footer>
</main>
<script nonce="__RELAY_CSP_NONCE__">
(function () {
  "use strict";

  var sessionToken = null;

  function b64u(buf) {
    var bytes = new Uint8Array(buf), s = "";
    for (var i = 0; i < bytes.length; i++) { s += String.fromCharCode(bytes[i]); }
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function unb64u(s) {
    var b = atob(s.replace(/-/g, "+").replace(/_/g, "/"));
    var out = new Uint8Array(b.length);
    for (var i = 0; i < b.length; i++) { out[i] = b.charCodeAt(i); }
    return out;
  }

  function post(path, body) {
    return fetch(path, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (data) {
        if (!r.ok) { throw new Error(data.error || ("refused with " + r.status)); }
        return data;
      });
    });
  }

  function say(text, bad) {
    var el = document.getElementById("status");
    el.textContent = text;
    el.className = bad ? "bad" : "";
  }

  function busy(on) {
    document.getElementById("register").disabled = on;
    document.getElementById("signin").disabled = on;
  }

  function register() {
    var code = document.getElementById("code").value.trim();
    if (!code) { say("Run relay login enrol and paste the code first.", true); return; }
    say("Waiting for your authenticator…");
    busy(true);
    post("/relay/login/challenge", { ceremony: "register" }).then(function (ch) {
      return navigator.credentials.create({
        publicKey: {
          challenge: unb64u(ch.challenge),
          rp: { id: ch.rp_id, name: "relay" },
          user: { id: unb64u(ch.user_handle), name: ch.user_name, displayName: ch.user_name },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }],
          authenticatorSelection: { userVerification: "required", residentKey: "discouraged" },
          attestation: "none",
          timeout: 60000
        }
      });
    }).then(function (cred) {
      return post("/relay/login/verify", {
        ceremony: "register",
        code: code,
        client_data_json: b64u(cred.response.clientDataJSON),
        attestation_object: b64u(cred.response.attestationObject)
      });
    }).then(function (out) {
      document.getElementById("code").value = "";
      say("Registered " + out.name + ".\nSign in to get a credential.");
    }).catch(function (err) {
      say("Registration refused: " + err.message, true);
    }).then(function () { busy(false); });
  }

  function signIn() {
    say("Waiting for your authenticator…");
    busy(true);
    post("/relay/login/challenge", { ceremony: "assert" }).then(function (ch) {
      if (!ch.credentials.length) {
        throw new Error("no passkey is registered on this machine yet");
      }
      return navigator.credentials.get({
        publicKey: {
          challenge: unb64u(ch.challenge),
          rpId: ch.rp_id,
          allowCredentials: ch.credentials.map(function (id) {
            return { type: "public-key", id: unb64u(id) };
          }),
          userVerification: "required",
          timeout: 60000
        }
      });
    }).then(function (assertion) {
      var r = assertion.response;
      return post("/relay/login/verify", {
        ceremony: "assert",
        credential_id: b64u(assertion.rawId),
        client_data_json: b64u(r.clientDataJSON),
        authenticator_data: b64u(r.authenticatorData),
        signature: b64u(r.signature),
        user_handle: r.userHandle ? b64u(r.userHandle) : ""
      });
    }).then(function (out) {
      sessionToken = out.token;
      say("Signed in. This browser holds a " + out.classes.join("+") +
          " credential until " + out.expires + ".");
    }).catch(function (err) {
      sessionToken = null;
      say("Sign-in refused: " + err.message, true);
    }).then(function () { busy(false); });
  }

  document.getElementById("register").addEventListener("click", register);
  document.getElementById("signin").addEventListener("click", signIn);
})();
</script>
</body>
</html>
`
