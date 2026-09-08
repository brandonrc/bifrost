"""A least-privilege identity for the scraper: local user `prometheus` (viewer), one 90-day PAT, stored as a Secret."""
import json, subprocess, urllib.request, base64, secrets
def k(*a, inp=None): return subprocess.run(["microk8s","kubectl",*a], input=inp, capture_output=True, text=True, check=True).stdout
pw=base64.b64decode(k("get","secret","-n","bifrost","bifrost-local-admin","-o","jsonpath={.data.BIFROST_LOCAL_ADMIN_PASSWORD}")).decode()
ip=k("get","svc","-n","bifrost","bifrost","-o","jsonpath={.spec.clusterIP}"); base=f"http://{ip}:8484"
def call(m,p,b=None,tok=None):
    r=urllib.request.Request(base+p,data=json.dumps(b).encode() if b is not None else None,method=m,headers={"Content-Type":"application/json",**({"Authorization":"Bearer "+tok} if tok else {})})
    try:
        with urllib.request.urlopen(r,timeout=20) as resp:
            raw=resp.read()
            try: return resp.status, json.loads(raw or b"null")
            except ValueError: return resp.status, raw.decode()[:300]
    except urllib.error.HTTPError as e: return e.code, e.read().decode()[:200]
_,l=call("POST","/api/v1/auth/login",{"username":"admin","password":pw}); admin=l["token"]
ppw=secrets.token_urlsafe(24)
st,body=call("POST","/api/v1/auth/users",{"username":"metrics-scraper","password":ppw,"role":"viewer","email":None},tok=admin)
print("create user:",st)
if st==409:
    # exists from an earlier attempt: rotate its password is not exposed; mint with admin-reset unsupported -> reuse via delete+create
    st,_=call("DELETE","/api/v1/auth/users/metrics-scraper",tok=admin); print("delete old:",st)
    st,body=call("POST","/api/v1/auth/users",{"username":"metrics-scraper","password":ppw,"role":"viewer","email":None},tok=admin); print("recreate:",st)
st,l=call("POST","/api/v1/auth/login",{"username":"metrics-scraper","password":ppw}); print("login as prometheus:",st)
st,t=call("POST","/api/v1/auth/tokens",{"label":"prometheus-scrape","expires_in_days":90},tok=l["token"]); print("mint PAT:",st)
pat=t["token"] if isinstance(t,dict) and "token" in t else (t.get("plaintext") if isinstance(t,dict) else None)
if not pat: print("token body keys:", t); raise SystemExit(1)
st,me=call("GET","/api/v1/metrics",tok=pat); print("metrics as prometheus:",st, (me if isinstance(me,str) else str(me))[:80])
k("create","namespace","observability","--dry-run=client","-o","yaml")
subprocess.run(["sh","-c",f"microk8s kubectl -n observability create secret generic bifrost-metrics-token --from-literal=token='{pat}' --dry-run=client -o yaml | microk8s kubectl apply -f -"],check=True)
print("secret applied")
