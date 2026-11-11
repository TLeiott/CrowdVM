"""``boat_sdk.models``: every request model, also as ``boat_sdk.models.<snake_name>`` modules
(``from boat_sdk.models.fork_request import ForkRequest``)."""
import importlib.abc
import importlib.util
import sys
import types

from crowdvm import models as _models


def __getattr__(name):
    return getattr(_models, name)


class _ModelModules(importlib.abc.MetaPathFinder, importlib.abc.Loader):
    def find_spec(self, fullname, path=None, target=None):
        if fullname.startswith(__name__ + ".") and fullname.count(".") == 2:
            return importlib.util.spec_from_loader(fullname, self)
        return None

    def create_module(self, spec):
        mod = types.ModuleType(spec.name)
        mod.__getattr__ = lambda name: getattr(_models, name)
        snake = spec.name.rsplit(".", 1)[1]
        pascal = "".join(p[:1].upper() + p[1:] for p in snake.split("_"))
        try:
            setattr(mod, pascal, getattr(_models, pascal))
        except AttributeError:
            pass
        return mod

    def exec_module(self, module):
        return None


if not any(isinstance(f, _ModelModules) for f in sys.meta_path):
    sys.meta_path.append(_ModelModules())
