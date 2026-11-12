// What a PII type or a mask type is called on screen. The scanner's types are
// PIIType in llm-service/src/agents/pii_scanner/ml_detector.py; a type this map
// does not name is spelled from its key, so a new one still reads as words.
const PII_TYPE_LABELS: Record<string, string> = {
  email: "Email",
  phone: "Phone",
  ssn: "SSN",
  credit_card: "Credit card",
  name: "Name",
  address: "Address",
  ip_address: "IP address",
  date_of_birth: "Date of birth",
  bank_account: "Bank account",
  passport: "Passport",
  driver_license: "Driver license",
  medical_license: "Medical license",
  url: "URL",
  location: "Location",
  date_time: "Date and time",
  person: "Person",
  organization: "Organization",
  password: "Password",
  unknown: "Unknown",
  "*": "All types",
};

// The engine's mask_type (lib/pii/masked-columns.ts): hash, redact, partial, ...
const MASK_TYPE_LABELS: Record<string, string> = {
  hash: "Hashed",
  redact: "Redacted",
  partial: "Partly masked",
};

function spell(key: string): string {
  const words = key.replace(/[_-]+/g, " ").trim();
  return words ? words[0].toUpperCase() + words.slice(1) : "Unknown";
}

export function piiTypeLabel(type: string): string {
  return PII_TYPE_LABELS[type] ?? spell(type);
}

export function maskTypeLabel(action: string): string {
  return MASK_TYPE_LABELS[action] ?? spell(action);
}
