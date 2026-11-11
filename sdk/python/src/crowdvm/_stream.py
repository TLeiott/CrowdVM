"""Interrupt a blocking stream read when its async consumer is cancelled."""
import contextvars
import socket

responses = contextvars.ContextVar("stream_responses", default=None)
#: a threading.Event that an async facade sets when the awaiting task is cancelled (polling helpers stop on it)
cancelled = contextvars.ContextVar("cancelled", default=None)


def set_read_timeout(resp, timeout):
    """Change the socket timeout of an open response (the one given to `open` also covered the headers)."""
    try:
        resp.fp.raw._sock.settimeout(timeout)
    except (AttributeError, OSError):
        pass


def interrupt(resp):
    try:
        resp.fp.raw._sock.shutdown(socket.SHUT_RDWR)
    except (AttributeError, OSError):
        pass
