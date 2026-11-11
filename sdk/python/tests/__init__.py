import os
import sys

# make `python3 -m unittest` work from sdk/python without installing the package
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "src"))
