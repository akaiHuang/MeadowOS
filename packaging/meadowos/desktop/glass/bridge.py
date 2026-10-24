"""Direct-script entry for native GTK consumers; see server.py for the protocol."""
from pathlib import Path
import sys

if __package__ in (None, ""):
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
    from glass.server import main
else:
    from .server import main


if __name__ == "__main__":
    main()
