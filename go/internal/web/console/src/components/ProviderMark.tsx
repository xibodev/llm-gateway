import { providerBrandID, resolveProviderBrand } from "./provider-brand-marks";
import "./provider-marks.css";

export { hasProviderMark, hasProviderBrand, providerBrandID, providerBrandForEndpoint, resolveProviderBrand } from "./provider-brand-marks";

function initials(value: string): string {
  const words = value.match(/[\p{L}\p{N}]+/gu) ?? [];
  return (words.length > 1 ? words.slice(0, 2).map((word) => Array.from(word)[0]).join("") : Array.from(words[0] ?? "?").slice(0, 2).join("")).toUpperCase();
}

export function ProviderMark({ id, label, baseURL }: { id: string; label?: string; baseURL?: string }) {
  const brand = resolveProviderBrand({ id, baseURL });
  const name = label?.trim() || id;
  const icon = brand?.icon;
  const paths = brand?.paths ?? (icon ? [{ d: icon.path }] : undefined);
  return (
    <span class={`provider-mark provider-mark--local${brand?.monochrome ? " provider-mark--monochrome" : ""}${brand?.paleBackground ? " provider-mark--pale" : ""}`} role="img" aria-label={name} data-provider-brand={providerBrandID(id, baseURL)}>
      {paths
        ? <svg viewBox={brand?.viewBox ?? "0 0 24 24"} aria-hidden="true" focusable="false" style={icon && !brand?.monochrome ? { color: `#${icon.hex}` } : undefined}>
          {paths.map((path) => <path key={path.d} fill={path.fill ?? "currentColor"} fill-rule={path.fillRule} d={path.d} />)}
        </svg>
        : <span class="provider-mark__initials" aria-hidden="true">{brand?.initials ?? initials(name)}</span>}
    </span>
  );
}
