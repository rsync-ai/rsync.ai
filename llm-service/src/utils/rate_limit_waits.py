"""Waiting out a model provider's 429 before reporting it.

A 429 is the provider out of capacity: Vertex answers Gemini calls with
RESOURCE_EXHAUSTED, no Retry-After, in spells that come and go with Google's
shared load (6 of 20 calls one minute, none the next, 2026-10-11). The SDK
client retries once, about half a second later; past that a call waits about
1, 2, 4, 8, then 10 seconds, or what the provider's Retry-After asks when that
is at most 10, before it reports the 429. That is at most 25 seconds asleep, so
a spell can pass inside a question's 120 (api-gateway's assistant Deadline).
Each wait is cut by up to half at random, so calls refused together do not all
come back together. Nothing else is tried again here: the client already
retried a 5xx, and a timeout has spent much of the caller's time.

One request's model calls share one :class:`RateLimitWaits`, so a draft and its
repairs together sleep no longer than one call may.
"""

from __future__ import annotations

import asyncio
import logging
import random
from typing import Awaitable, Callable, Optional, TypeVar

logger = logging.getLogger(__name__)

WAITS = (1.0, 2.0, 4.0, 8.0, 10.0)
RETRY_AFTER_MOST = 10.0

T = TypeVar("T")

# asyncio.sleep by a name tests can replace without stopping every other sleep.
_sleep = asyncio.sleep


def rate_limit_wait(exc: BaseException, retries: int) -> Optional[float]:
    """Seconds to wait before trying a call that raised ``exc`` again, or None."""
    if getattr(exc, "status_code", None) != 429 or retries >= len(WAITS):
        return None
    headers = getattr(getattr(exc, "response", None), "headers", None) or {}
    try:
        asked = float(headers.get("retry-after") or "")
    except ValueError:
        # Absent, or an HTTP date.
        return WAITS[retries] * (1.0 - random.random() / 2)
    return max(asked, 0.0) if asked <= RETRY_AFTER_MOST else None


class RateLimitWaits:
    """The 429 waits one request may still spend, across all its model calls."""

    def __init__(self, most: float = sum(WAITS)):
        self.most = most
        self.retries = 0
        self.slept = 0.0

    def next(self, exc: BaseException) -> Optional[float]:
        """The wait before trying again after ``exc``, or None to report it."""
        wait = rate_limit_wait(exc, self.retries)
        if wait is None or self.slept + wait > self.most:
            return None
        self.retries += 1
        self.slept += wait
        return wait


async def wait_out(call: Callable[[], Awaitable[T]], waits: RateLimitWaits, what: str) -> T:
    """``await call()``, again after each 429 that ``waits`` still has time for."""
    while True:
        try:
            return await call()
        except Exception as exc:
            wait = waits.next(exc)
            if wait is None:
                raise
            logger.info("%s rate limited: retry=%d wait=%.1fs", what, waits.retries, wait)
            await _sleep(wait)
