"""Read an early HTTP error even when the peer rejects the request body."""
import http.client
import urllib.parse
import urllib.request


class _EarlyResponse:
    def request(self, *args, **kwargs):
        try:
            super().request(*args, **kwargs)
        except (BrokenPipeError, ConnectionResetError):
            # A server can send its error before consuming an oversized body.
            # getresponse still raises if the peer supplied no HTTP response.
            pass


class _HTTPConnection(_EarlyResponse, http.client.HTTPConnection):
    pass


class _HTTPSConnection(_EarlyResponse, http.client.HTTPSConnection):
    pass


class _HTTPHandler(urllib.request.HTTPHandler):
    def http_open(self, req):
        return self.do_open(_HTTPConnection, req)


class _HTTPSHandler(urllib.request.HTTPSHandler):
    def https_open(self, req):
        return self.do_open(_HTTPSConnection, req, context=self._context)


class _SameOriginRedirect(urllib.request.HTTPRedirectHandler):
    """Do not forward credentials to another origin (browsers' fetch strips them too)."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        if new is not None:
            a, b = urllib.parse.urlsplit(req.full_url), urllib.parse.urlsplit(new.full_url)
            if (a.scheme, a.netloc) != (b.scheme, b.netloc):
                new.remove_header("Authorization")
        return new


opener = urllib.request.build_opener(_HTTPHandler(), _HTTPSHandler(), _SameOriginRedirect())
