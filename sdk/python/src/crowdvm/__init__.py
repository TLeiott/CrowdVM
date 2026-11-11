"""CrowdVM Python SDK: persistent Ubuntu VMs for coding agents on your LAN or Tailnet.

    from crowdvm import CrowdVM, wait_until_ready, run_command

    cvm = CrowdVM()   # CROWDVM_API_KEY, CROWDVM_API_BASE
    sb = cvm.create(ttl_seconds=1800).sandbox
    wait_until_ready(cvm, sb.id)
    print(cvm.command(sb.id, "echo hi").stdout)
    cvm.stop(sb.id)
"""
from ._util import UNSET, Obj
from .aio import AsyncCrowdVM
from .client import DEFAULT_BASE_URL, VERSION, CrowdVM
from .compat import ApiClient, BoatApi, Configuration
from .errors import (APIConnectionError, ApiError, ApiException, AuthenticationError, BadRequestError, ConflictError,
                     CrowdVMError, NotFoundError, PaymentRequiredError, PermissionDeniedError, PromptFailed,
                     RateLimitError, SandboxFailed, ServerError, ServiceUnavailableError, WaitTimeout)
from .helpers import (exec_command, follow_command, paginate, read_text, run_command, stop_and_remove, stream_command,
                      stream_events, stream_prompt, wait_for_desktop, wait_for_prompt, wait_for_prompt_done,
                      wait_until_idle, wait_until_ready, write_text)
from .webhooks import WebhookVerificationError, construct_event, sign_webhook, verify_webhook

__version__ = VERSION
Client = CrowdVM
AsyncClient = AsyncCrowdVM

__all__ = [n for n in dir() if not n.startswith("_")]
