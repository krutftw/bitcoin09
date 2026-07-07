#!/usr/bin/env node

import { mkdir, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";

const repo = process.env.GITHUB_REPOSITORY || "krutftw/bitcoin09";
const outputPath = resolve(process.argv[2] || "docs/market-data.json");
const apiBase = `https://api.github.com/repos/${repo}/issues`;

function authHeaders() {
  const headers = {
    Accept: "application/vnd.github+json",
    "X-GitHub-Api-Version": "2022-11-28",
    "User-Agent": "bitcoin09-market-data-builder",
  };
  if (process.env.GITHUB_TOKEN) {
    headers.Authorization = `Bearer ${process.env.GITHUB_TOKEN}`;
  }
  return headers;
}

async function fetchIssues(labels, state) {
  const issues = [];
  let page = 1;

  while (page <= 10) {
    const url = new URL(apiBase);
    url.searchParams.set("state", state);
    url.searchParams.set("labels", labels);
    url.searchParams.set("per_page", "100");
    url.searchParams.set("page", String(page));

    const response = await fetch(url, { headers: authHeaders() });
    if (!response.ok) {
      throw new Error(`GitHub ${labels} request failed: ${response.status} ${await response.text()}`);
    }

    const batch = (await response.json()).filter((issue) => !issue.pull_request);
    issues.push(...batch);
    if (batch.length < 100) break;
    page += 1;
  }

  return issues;
}

function escapeRegExp(value) {
  return String(value).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

function issueSection(body, heading) {
  const re = new RegExp(`### ${escapeRegExp(heading)}\\s*\\n+([\\s\\S]*?)(?=\\n### |$)`, "i");
  const match = String(body ?? "").match(re);
  if (!match) return "";
  const text = match[1].replace(/<!--.*?-->/gs, "").trim();
  return text === "_No response_" ? "" : text;
}

function numeric(value) {
  const match = String(value ?? "").replaceAll(",", "").match(/-?\d+(\.\d+)?/);
  return match ? Number(match[0]) : null;
}

function sideFor(issue) {
  const labels = issue.labels.map((label) => label.name);
  if (labels.includes("otc-buy") || issue.title.toUpperCase().includes("WTB")) return "buy";
  if (labels.includes("otc-sell") || issue.title.toUpperCase().includes("WTS")) return "sell";
  return "offer";
}

function rowFromIssue(issue, completed) {
  const kind = completed ? "completed" : sideFor(issue);
  const amountText = issueSection(issue.body, "09C amount") || issueSection(issue.body, "Amount traded");
  const priceText = issueSection(issue.body, "Total offer price") || issueSection(issue.body, "Total trade price");
  const amount = numeric(amountText);
  const total = numeric(priceText);
  const pricePer09c = amount && amount > 0 && total != null ? total / amount : null;

  return {
    number: issue.number,
    title: issue.title,
    url: issue.html_url,
    state: issue.state,
    kind,
    author: issue.user?.login ?? "",
    createdAt: issue.created_at,
    updatedAt: issue.updated_at,
    closedAt: issue.closed_at,
    amountText,
    priceText,
    paymentAsset: issueSection(issue.body, "Payment asset"),
    contact: issueSection(issue.body, "Public contact") || issueSection(issue.body, "Public parties"),
    proof: issueSection(issue.body, "Public chain proof"),
    notes: issueSection(issue.body, "Terms or notes") || issueSection(issue.body, "Public notes"),
    amount,
    pricePer09c,
  };
}

const [offerIssues, completedIssues] = await Promise.all([
  fetchIssues("otc-offer", "open"),
  fetchIssues("otc-completed", "all"),
]);

const offers = offerIssues.map((issue) => rowFromIssue(issue, false));
const completed = completedIssues.map((issue) => rowFromIssue(issue, true));

const payload = {
  generatedAt: new Date().toISOString(),
  source: `https://github.com/${repo}/issues?q=label%3Aotc-offer+OR+label%3Aotc-completed`,
  counts: {
    openOffers: offers.length,
    completed: completed.length,
  },
  offers,
  completed,
};

await mkdir(dirname(outputPath), { recursive: true });
await writeFile(outputPath, JSON.stringify(payload, null, 2) + "\n", "utf8");
console.log(`Wrote ${outputPath} (${offers.length} open offers, ${completed.length} completed records)`);
