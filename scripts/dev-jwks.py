#!/usr/bin/env python3
"""Local stand-in for Firebase sign-in, for development only.

It serves a signing key at http://127.0.0.1:PORT (the API's FIREBASE_JWKS_URL) and writes ready-made
customer tokens into .dev-tokens/. Point the API at it:

    FIREBASE_PROJECT_ID=dev
    FIREBASE_JWKS_URL=http://127.0.0.1:55996

then call the API with  -H "Authorization: Bearer $(cat .dev-tokens/rina)".

Needs:  pip install pyjwt cryptography
Never use this outside a developer machine: anyone who runs it can mint tokens.
"""
import base64, http.server, json, os, sys, time

import jwt
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

PORT = int(os.environ.get("DEV_JWKS_PORT", "55996"))
PROJECT = os.environ.get("FIREBASE_PROJECT_ID", "dev")
USERS = {  # name -> (firebase uid, phone, display name)
    "rina": ("uid-rina", "+628123456789", "Rina Sari"),
    "budi": ("uid-budi", "+628111111111", "Budi"),
}

key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
pem = key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption())
numbers = key.public_key().public_numbers()


def b64(n: int) -> str:
    return base64.urlsafe_b64encode(n.to_bytes((n.bit_length() + 7) // 8, "big")).rstrip(b"=").decode()


JWKS = {"keys": [{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "dev", "n": b64(numbers.n), "e": b64(numbers.e)}]}


def token(uid: str, phone: str, name: str) -> str:
    now = int(time.time())
    claims = {
        "iss": f"https://securetoken.google.com/{PROJECT}", "aud": PROJECT, "sub": uid,
        "iat": now, "exp": now + 24 * 3600, "auth_time": now - 60, "phone_number": phone, "name": name,
    }
    return jwt.encode(claims, pem, algorithm="RS256", headers={"kid": "dev"})


os.makedirs(".dev-tokens", exist_ok=True)
for who, (uid, phone, name) in USERS.items():
    with open(f".dev-tokens/{who}", "w") as f:
        f.write(token(uid, phone, name))
print(f"keys on http://127.0.0.1:{PORT}, project '{PROJECT}', tokens in .dev-tokens/ ({', '.join(USERS)}), valid 24h", flush=True)


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps(JWKS).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Cache-Control", "max-age=3600")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


try:
    http.server.ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
except KeyboardInterrupt:
    sys.exit(0)
