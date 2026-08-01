export const SUPPORTER_TIERS = Object.freeze([
  Object.freeze({
    key: "supporter",
    minUsd: 5,
    label: "Supporter",
    roleName: "💛 Supporter",
    color: 0xe0ac35,
    supporterLab: false,
  }),
  Object.freeze({
    key: "backer",
    minUsd: 25,
    label: "Backer",
    roleName: "🤝 Backer",
    color: 0x2d9cdb,
    supporterLab: true,
  }),
  Object.freeze({
    key: "builder",
    minUsd: 100,
    label: "Builder",
    roleName: "🛠 Builder",
    color: 0x9b51e0,
    supporterLab: true,
  }),
  Object.freeze({
    key: "core_supporter",
    minUsd: 250,
    label: "Core Supporter",
    roleName: "⭐ Core Supporter",
    color: 0xe67e22,
    supporterLab: true,
  }),
]);

export function supporterTierFor(amountUsd) {
  const amount = Number(amountUsd);
  if (!Number.isFinite(amount)) return null;
  return SUPPORTER_TIERS.filter((tier) => amount >= tier.minUsd).at(-1) ?? null;
}

export function publicSupporterTier(tier) {
  if (!tier) return null;
  return {
    key: tier.key,
    min_usd: tier.minUsd,
    label: tier.label,
    role_name: tier.roleName,
    supporter_lab: tier.supporterLab,
  };
}
