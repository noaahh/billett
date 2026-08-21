const maximumEmailBytes = 25 * 1024 * 1024;
const receiptSubjects = ["easyride kaufquittung", "ihr online-kauf bei der sbb"];

export default {
  async email(message, env) {
    requireSetting(env.BILLETT_INBOUND_URL, "BILLETT_INBOUND_URL");
    requireSetting(env.BILLETT_INBOUND_TOKEN, "BILLETT_INBOUND_TOKEN");

    const allowedRecipient = normalizedAddress(env.ALLOWED_TO);
    if (allowedRecipient && normalizedAddress(message.to) !== allowedRecipient) {
      message.setReject("This address is not configured for journey receipts");
      return;
    }

    const subject = message.headers.get("subject")?.toLocaleLowerCase() || "";
    if (!receiptSubjects.some((candidate) => subject.includes(candidate))) {
      const fallback = normalizedAddress(env.FALLBACK_FORWARD_TO);
      if (fallback) {
        await message.forward(fallback);
        return;
      }
      message.setReject("Only supported SBB purchase receipts are accepted");
      return;
    }

    const senderDomain = normalizedAddress(message.from).split("@").pop();
    const allowedSenderDomains = String(env.ALLOWED_FROM_DOMAINS || "sbb.ch")
      .split(",")
      .map((value) => value.trim().toLocaleLowerCase())
      .filter(Boolean);
    if (!senderDomain || !allowedSenderDomains.some((domain) => senderDomain === domain || senderDomain.endsWith(`.${domain}`))) {
      message.setReject("Receipt sender domain is not allowed");
      return;
    }

    if (message.rawSize > maximumEmailBytes) {
      message.setReject("Email exceeds the 25 MiB ingestion limit");
      return;
    }

    const rawEmail = await new Response(message.raw).arrayBuffer();
    const response = await fetch(env.BILLETT_INBOUND_URL, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${env.BILLETT_INBOUND_TOKEN}`,
        "Content-Type": "message/rfc822",
        "X-Email-Filename": "cloudflare-email.eml",
      },
      body: rawEmail,
      redirect: "error",
    });
    if (!response.ok) {
      const details = (await response.text()).slice(0, 500);
      throw new Error(`billett ingestion returned ${response.status}: ${details}`);
    }
  },
};

function normalizedAddress(value) {
  return String(value || "").trim().toLocaleLowerCase();
}

function requireSetting(value, name) {
  if (!String(value || "").trim()) throw new Error(`${name} is not configured`);
}
