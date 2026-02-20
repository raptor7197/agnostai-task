"""
Event Analytics SDK - Python Client

A lightweight client library for the Event Analytics Pipeline API.
Allows developers to easily track, update, and delete events,
as well as query analytics data.

Usage:
    from event_client import EventClient

    client = EventClient("http://localhost:8080")

    # Track a new event
    event_id = client.track_event(
        conversation_id="conv_123",
        event_name="event_a",
        input="user query",
        output="bot response",
        latency=123.45,
    )

    # Update an event
    client.update_event(event_id, error="timeout", latency=5000.0)

    # Delete an event
    client.delete_event(event_id)

    # Query analytics
    errors = client.get_error_counts("event_a", days=7)
    latency = client.get_latency_stats(limit=50)
    sessions = client.get_top_error_sessions(limit=10)
"""

from __future__ import annotations

import json
import logging
import time
from typing import Any, Optional
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

logger = logging.getLogger("event_sdk")


class EventSDKError(Exception):
    """Base exception for the Event SDK."""

    def __init__(
        self, message: str, status_code: int | None = None, response: dict | None = None
    ):
        super().__init__(message)
        self.status_code = status_code
        self.response = response


class EventNotFoundError(EventSDKError):
    """Raised when an event is not found."""

    pass


class EventValidationError(EventSDKError):
    """Raised when request validation fails."""

    pass


class EventClient:
    """
    Client for the Event Analytics Pipeline API.

    This client wraps the REST API and provides a clean interface for:
    - Tracking (creating) events
    - Updating events
    - Deleting events
    - Reading events and analytics data

    All write operations (create/update/delete) are asynchronous — they go through
    Kafka and are processed by the consumer. The returned event ID is generated
    immediately, but the event may not be queryable for a brief period.

    All read operations go directly to ClickHouse and return immediately.
    """

    VALID_EVENT_NAMES = {"event_a", "event_b", "event_c"}

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        timeout: float = 30.0,
        max_retries: int = 3,
        retry_delay: float = 1.0,
    ):
        """
        Initialize the Event Analytics client.

        Args:
            base_url: Base URL of the API server (e.g., "http://localhost:8080")
            timeout: HTTP request timeout in seconds
            max_retries: Maximum number of retries for failed requests
            retry_delay: Delay between retries in seconds (doubles each retry)
        """
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.max_retries = max_retries
        self.retry_delay = retry_delay

    def _request(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        params: dict | None = None,
    ) -> dict:
        """
        Make an HTTP request to the API with retry logic.

        Args:
            method: HTTP method (GET, POST, PUT, DELETE)
            path: API path (e.g., "/event")
            body: JSON request body
            params: URL query parameters

        Returns:
            Parsed JSON response as a dictionary

        Raises:
            EventSDKError: On API errors
            EventNotFoundError: When resource is not found (404)
            EventValidationError: When validation fails (400)
        """
        url = self.base_url + path
        if params:
            url += "?" + urlencode({k: v for k, v in params.items() if v is not None})

        data = None
        if body is not None:
            data = json.dumps(body).encode("utf-8")

        last_error = None
        for attempt in range(self.max_retries):
            try:
                req = Request(url, data=data, method=method)
                req.add_header("Content-Type", "application/json")
                req.add_header("Accept", "application/json")

                with urlopen(req, timeout=self.timeout) as resp:
                    response_body = resp.read().decode("utf-8")
                    if response_body:
                        return json.loads(response_body)
                    return {"success": True}

            except HTTPError as e:
                response_body = e.read().decode("utf-8")
                try:
                    error_data = json.loads(response_body)
                except (json.JSONDecodeError, ValueError):
                    error_data = {"error": response_body}

                error_msg = error_data.get("error", f"HTTP {e.code}")

                if e.code == 404:
                    raise EventNotFoundError(
                        error_msg, status_code=e.code, response=error_data
                    )
                elif e.code == 400:
                    raise EventValidationError(
                        error_msg, status_code=e.code, response=error_data
                    )
                elif e.code >= 500:
                    last_error = EventSDKError(
                        error_msg, status_code=e.code, response=error_data
                    )
                    if attempt < self.max_retries - 1:
                        delay = self.retry_delay * (2**attempt)
                        logger.warning(
                            "Request failed (attempt %d/%d), retrying in %.1fs: %s",
                            attempt + 1,
                            self.max_retries,
                            delay,
                            error_msg,
                        )
                        time.sleep(delay)
                        continue
                    raise last_error
                else:
                    raise EventSDKError(
                        error_msg, status_code=e.code, response=error_data
                    )

            except URLError as e:
                last_error = EventSDKError(f"Connection error: {e.reason}")
                if attempt < self.max_retries - 1:
                    delay = self.retry_delay * (2**attempt)
                    logger.warning(
                        "Connection failed (attempt %d/%d), retrying in %.1fs: %s",
                        attempt + 1,
                        self.max_retries,
                        delay,
                        e.reason,
                    )
                    time.sleep(delay)
                    continue
                raise last_error

            except Exception as e:
                last_error = EventSDKError(f"Unexpected error: {e}")
                if attempt < self.max_retries - 1:
                    delay = self.retry_delay * (2**attempt)
                    time.sleep(delay)
                    continue
                raise last_error

        raise last_error or EventSDKError("Request failed after all retries")

    # ==================== Write Operations (via Kafka) ====================

    def track_event(
        self,
        conversation_id: str,
        event_name: str,
        error: str = "",
        latency: float = 0.0,
        input: str = "",
        input_embeddings: list[float] | None = None,
        output: str = "",
        output_embeddings: list[float] | None = None,
        conversation_metadata: str = "",
    ) -> str:
        """
        Track (create) a new event. This is the primary ingestion method.

        The event is published to Kafka and will be written to ClickHouse
        asynchronously by the consumer. The event ID is returned immediately.

        Args:
            conversation_id: ID of the conversation/session this event belongs to
            event_name: Type of event ("event_a", "event_b", or "event_c")
            error: Error message if the event represents an error (empty string = no error)
            latency: Latency in milliseconds
            input: Input text/data for the event
            input_embeddings: Vector embeddings for the input
            output: Output text/data for the event
            output_embeddings: Vector embeddings for the output
            conversation_metadata: JSON string with conversation metadata

        Returns:
            The generated event ID (UUID string)

        Raises:
            EventValidationError: If required fields are missing or invalid
            EventSDKError: On API errors
        """
        if not conversation_id:
            raise EventValidationError("conversation_id is required")
        if event_name not in self.VALID_EVENT_NAMES:
            raise EventValidationError(
                f"event_name must be one of {self.VALID_EVENT_NAMES}, got '{event_name}'"
            )

        body: dict[str, Any] = {
            "conversation_id": conversation_id,
            "event_name": event_name,
            "error": error,
            "latency": latency,
            "input": input,
            "output": output,
        }

        if input_embeddings:
            body["input_embeddings"] = input_embeddings
        if output_embeddings:
            body["output_embeddings"] = output_embeddings
        if conversation_metadata:
            body["conversation_metadata"] = conversation_metadata

        resp = self._request("POST", "/event/", body=body)
        data = resp.get("data", {})
        event_id = data.get("id", "")

        if not event_id:
            raise EventSDKError("API did not return an event ID", response=resp)

        logger.debug(
            "Tracked event %s (name=%s, conv=%s)", event_id, event_name, conversation_id
        )
        return event_id

    def update_event(
        self,
        event_id: str,
        event_name: str | None = None,
        error: str | None = None,
        latency: float | None = None,
        input: str | None = None,
        input_embeddings: list[float] | None = None,
        output: str | None = None,
        output_embeddings: list[float] | None = None,
        conversation_metadata: str | None = None,
    ) -> dict:
        """
        Update an existing event. Only provided fields will be updated.

        The update is published to Kafka and processed asynchronously.

        Args:
            event_id: ID of the event to update
            event_name: New event name (optional)
            error: New error message (optional)
            latency: New latency value (optional)
            input: New input text (optional)
            input_embeddings: New input embeddings (optional)
            output: New output text (optional)
            output_embeddings: New output embeddings (optional)
            conversation_metadata: New metadata (optional)

        Returns:
            API response dictionary

        Raises:
            EventValidationError: If event_name is invalid
            EventSDKError: On API errors
        """
        if not event_id:
            raise EventValidationError("event_id is required")

        if event_name is not None and event_name not in self.VALID_EVENT_NAMES:
            raise EventValidationError(
                f"event_name must be one of {self.VALID_EVENT_NAMES}, got '{event_name}'"
            )

        body: dict[str, Any] = {}
        if event_name is not None:
            body["event_name"] = event_name
        if error is not None:
            body["error"] = error
        if latency is not None:
            body["latency"] = latency
        if input is not None:
            body["input"] = input
        if input_embeddings is not None:
            body["input_embeddings"] = input_embeddings
        if output is not None:
            body["output"] = output
        if output_embeddings is not None:
            body["output_embeddings"] = output_embeddings
        if conversation_metadata is not None:
            body["conversation_metadata"] = conversation_metadata

        if not body:
            raise EventValidationError("At least one field must be provided for update")

        resp = self._request("PUT", f"/event/{event_id}", body=body)
        logger.debug("Updated event %s", event_id)
        return resp

    def delete_event(self, event_id: str) -> dict:
        """
        Delete an event (soft delete via Kafka).

        The delete is published to Kafka and processed asynchronously.
        The event will be marked as deleted but not physically removed.

        Args:
            event_id: ID of the event to delete

        Returns:
            API response dictionary

        Raises:
            EventSDKError: On API errors
        """
        if not event_id:
            raise EventValidationError("event_id is required")

        resp = self._request("DELETE", f"/event/{event_id}")
        logger.debug("Deleted event %s", event_id)
        return resp

    # ==================== Read Operations (direct from ClickHouse) ====================

    def get_event(self, event_id: str) -> dict | None:
        """
        Get a single event by ID.

        Reads directly from ClickHouse.

        Args:
            event_id: ID of the event to retrieve

        Returns:
            Event data dictionary, or None if not found

        Raises:
            EventSDKError: On API errors
        """
        if not event_id:
            raise EventValidationError("event_id is required")

        try:
            resp = self._request("GET", f"/event/{event_id}")
            return resp.get("data")
        except EventNotFoundError:
            return None

    def get_events_by_conversation(self, conversation_id: str) -> list[dict]:
        """
        Get all events for a conversation, ordered by creation time.

        Reads directly from ClickHouse.

        Args:
            conversation_id: ID of the conversation/session

        Returns:
            List of event data dictionaries

        Raises:
            EventSDKError: On API errors
        """
        if not conversation_id:
            raise EventValidationError("conversation_id is required")

        resp = self._request("GET", f"/events/conversation/{conversation_id}")
        data = resp.get("data")
        return data if data is not None else []

    # ==================== Analytics Operations ====================

    def get_error_counts(self, event_name: str, days: int = 7) -> list[dict]:
        """
        Get error counts per day for a given event name.

        Answers: "How many errors in event_a over the last 7 days?"

        Args:
            event_name: Event type to query
            days: Number of days to look back (default: 7)

        Returns:
            List of dicts with keys: date, event_name, error_count, total_count

        Raises:
            EventSDKError: On API errors
        """
        if event_name not in self.VALID_EVENT_NAMES:
            raise EventValidationError(
                f"event_name must be one of {self.VALID_EVENT_NAMES}, got '{event_name}'"
            )

        resp = self._request(
            "GET",
            "/analytics/errors",
            params={
                "event_name": event_name,
                "days": str(days),
            },
        )
        data = resp.get("data")
        return data if data is not None else []

    def get_latency_stats(self, limit: int = 100) -> list[dict]:
        """
        Get p95 latency per event type per session.

        Answers: "p95 latency per event type per session"

        Args:
            limit: Maximum number of results (default: 100)

        Returns:
            List of dicts with keys: conversation_id, event_name, p95_latency, event_count

        Raises:
            EventSDKError: On API errors
        """
        resp = self._request(
            "GET",
            "/analytics/latency",
            params={
                "limit": str(limit),
            },
        )
        data = resp.get("data")
        return data if data is not None else []

    def get_top_error_sessions(self, limit: int = 100) -> list[dict]:
        """
        Get top sessions ranked by error rate.

        Answers: "Top sessions by error rate"

        Args:
            limit: Maximum number of results (default: 100)

        Returns:
            List of dicts with keys: conversation_id, total_events, error_events, error_rate

        Raises:
            EventSDKError: On API errors
        """
        resp = self._request(
            "GET",
            "/analytics/sessions/top-errors",
            params={
                "limit": str(limit),
            },
        )
        data = resp.get("data")
        return data if data is not None else []

    # ==================== Utility ====================

    def health_check(self) -> bool:
        """
        Check if the API server is healthy.

        Returns:
            True if the server is healthy, False otherwise
        """
        try:
            resp = self._request("GET", "/health")
            return resp.get("success", False)
        except Exception:
            return False

    def wait_until_ready(self, timeout: float = 60.0, interval: float = 2.0) -> bool:
        """
        Wait until the API server is ready, with timeout.

        Args:
            timeout: Maximum time to wait in seconds
            interval: Time between health check attempts in seconds

        Returns:
            True if the server became ready, False if timed out
        """
        start = time.time()
        while time.time() - start < timeout:
            if self.health_check():
                return True
            time.sleep(interval)
        return False

    def __repr__(self) -> str:
        return f"EventClient(base_url='{self.base_url}')"


# ==================== Convenience Functions ====================


def create_client(
    base_url: str = "http://localhost:8080",
    timeout: float = 30.0,
    max_retries: int = 3,
) -> EventClient:
    """
    Convenience function to create an EventClient instance.

    Args:
        base_url: Base URL of the API server
        timeout: HTTP request timeout in seconds
        max_retries: Maximum number of retries for failed requests

    Returns:
        Configured EventClient instance
    """
    return EventClient(base_url=base_url, timeout=timeout, max_retries=max_retries)


if __name__ == "__main__":
    # Quick demo / smoke test
    logging.basicConfig(level=logging.DEBUG)

    client = EventClient("http://localhost:8080")

    if not client.health_check():
        print("API is not available. Start the server first.")
        exit(1)

    print("API is healthy!")

    # Track an event
    event_id = client.track_event(
        conversation_id="demo_conv_1",
        event_name="event_a",
        input="Hello, how are you?",
        output="I'm doing well, thank you!",
        latency=150.5,
        conversation_metadata='{"user_id": "demo_user"}',
    )
    print(f"Created event: {event_id}")

    # Give the consumer a moment to process
    time.sleep(2)

    # Read it back
    event = client.get_event(event_id)
    if event:
        print(f"Retrieved event: {json.dumps(event, indent=2)}")
    else:
        print("Event not found yet (consumer may still be processing)")

    # Update it
    client.update_event(event_id, error="simulated_error", latency=3000.0)
    print(f"Updated event: {event_id}")

    # Get analytics
    errors = client.get_error_counts("event_a", days=7)
    print(f"Error counts: {errors}")

    print("\nSDK demo complete!")
