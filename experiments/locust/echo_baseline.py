import os
from locust import HttpUser, task, between


TARGET_PATH = os.getenv("TARGET_PATH", "/echo")
REQUEST_METHOD = os.getenv("REQUEST_METHOD", "GET").upper()
PAYLOAD = os.getenv("PAYLOAD", "baseline")


class EchoBaselineUser(HttpUser):
    wait_time = between(0.01, 0.05)

    @task
    def hit_target(self):
        if REQUEST_METHOD == "POST":
            self.client.post(
                TARGET_PATH,
                data=PAYLOAD,
                headers={"Content-Type": "text/plain"},
                name=TARGET_PATH,
            )
            return

        self.client.get(TARGET_PATH, name=TARGET_PATH)
