# Validate YAML syntax of all repo manifests.
# NOTE: Helm template files (*.yaml under chart/ templates/) contain
# Go template syntax and are intentionally skipped here — they are
# validated by `helm lint` / `helm template` (Makefile, CI chart-lint).
import glob
import sys
import yaml

files = (
    glob.glob("config/**/*.yaml", recursive=True)
    + glob.glob("chart/k8s-sched/*.yaml")
    + ["config/rbac/rbac.yaml", ".github/workflows/ci.yml"]
)
ok = True
for f in sorted(set(files)):
    try:
        with open(f, encoding="utf-8") as fh:
            list(yaml.safe_load_all(fh))
        print("OK  ", f)
    except Exception as e:
        ok = False
        print("FAIL", f, "->", e)
sys.exit(0 if ok else 1)
