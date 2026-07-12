import { greeting } from "@/greeting/greeting.ts";

if (import.meta.main) {
  console.log(greeting("polyglot"));
}
