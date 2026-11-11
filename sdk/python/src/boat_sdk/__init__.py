"""Boat SDK import paths for CrowdVM: code written for ``boat_sdk`` imports unchanged.

``boat_sdk``, ``boat_sdk.api.boat_api``, ``boat_sdk.models`` (and ``boat_sdk.models.<name>``),
``boat_sdk.exceptions``, ``boat_sdk.api_client``, ``boat_sdk.configuration`` and ``boat_sdk.rest``
all resolve to the ``crowdvm`` package.  Point ``Configuration(host=...)`` at your CrowdVM server.
Do not install Boat's own ``boat-sdk`` next to this package: both provide ``boat_sdk``.
"""
from crowdvm import *  # noqa: F401,F403
from crowdvm import __all__ as _all, __version__  # noqa: F401
from crowdvm.compat import ApiClient, BoatApi, Configuration  # noqa: F401
from crowdvm.errors import ApiException  # noqa: F401

from . import models  # noqa: F401,E402

__all__ = list(_all) + ["models"]
