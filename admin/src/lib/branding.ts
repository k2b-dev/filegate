export const defaultAdminName = "Filegate Admin";

export function resolveAdminName(value: string | undefined): string {
  return value?.trim() || defaultAdminName;
}

// Branding is deployment metadata and therefore fixed when the admin process
// starts, just like the rest of its environment-based configuration.
export const adminName = resolveAdminName(Bun.env.ADMIN_INSTANCE_NAME);
