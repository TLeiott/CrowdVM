from crowdvm.errors import *  # noqa: F401,F403
from crowdvm.errors import (  # noqa: F401
    ApiError, ApiException, AuthenticationError, BadRequestError, ConflictError, CrowdVMError, NotFoundError,
    PermissionDeniedError, ServerError,
)

# Boat's generated SDK names: real subclasses, so `except NotFoundException:`
# catches only a 404 (the client raises the crowdvm per-status classes).
OpenApiException = CrowdVMError
BadRequestException = BadRequestError
UnauthorizedException = AuthenticationError
ForbiddenException = PermissionDeniedError
NotFoundException = NotFoundError
ConflictException = ConflictError
ServiceException = ServerError


class ApiTypeError(OpenApiException, TypeError): ...


class ApiValueError(OpenApiException, ValueError): ...


class ApiKeyError(OpenApiException, KeyError): ...


class ApiAttributeError(OpenApiException, AttributeError): ...
