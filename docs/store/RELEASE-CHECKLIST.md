# BTC09 Wallet store release checklist

Last checked: 16 July 2026

The code and unsigned preflight packages can be prepared before the publisher accounts exist. Do not publish a download or announce the mobile apps until a store-signed build has been installed and tested from the real store.

## Publisher accounts

BTC09 is a cryptocurrency wallet, so personal hobby accounts are the wrong account type.

- **Windows:** use a Microsoft Company account. Microsoft Store policy requires a Company account for cryptocurrency wallets and products that handle private keys or recovery phrases. Company registration is about US$99 once. Submit an MSIX package so Microsoft handles the trusted signature; an MSI or EXE submission still needs a separate trusted code-signing certificate.
- **Android:** use a Google Play Organization account. Google requires this account type for cryptocurrency software wallets. It costs US$25 once and requires a legal organization with a matching D-U-N-S number, address, phone number and payments profile.
- **iPhone and Mac:** use an Apple Developer Program organization membership. Apple requires an organization publisher for cryptocurrency wallets. Membership is US$99 per year and the organization must pass Apple and D-U-N-S verification.

If there is no eligible legal organization yet, stop at the unsigned preflight packages. Do not open personal publisher accounts and hope review misses the wallet category.

Official references:

- Microsoft Store crypto policy: https://learn.microsoft.com/en-us/windows/apps/publish/store-policies
- Microsoft Store signing choices: https://learn.microsoft.com/en-us/windows/apps/package-and-deploy/code-signing-options
- Google Play account types: https://support.google.com/googleplay/android-developer/answer/13634885
- Google Play wallet policy: https://support.google.com/googleplay/android-developer/answer/16329703
- Google Play registration: https://support.google.com/android-developer-console/answer/16604405
- Apple crypto review rules: https://developer.apple.com/app-store/review/guidelines/
- Apple membership: https://developer.apple.com/programs/whats-included/

## Android signing

Google Play App Signing is mandatory for a new app. Keep the app-signing key in Play and use a separate upload key for releases.

The Android Gradle project signs only when all four variables are present:

- `BTC09_ANDROID_KEYSTORE`
- `BTC09_ANDROID_KEYSTORE_PASSWORD`
- `BTC09_ANDROID_KEY_ALIAS`
- `BTC09_ANDROID_KEY_PASSWORD`

The build fails if only some are set. Store the upload keystore and passwords in encrypted release secrets, keep a separate offline backup, and never commit them. With no signing variables, `npm run mobile:android:build` produces an unsigned APK and AAB for preflight checks.

Before upload:

1. Enrol the app in Play App Signing.
2. Generate one permanent upload key and back it up offline.
3. Build the signed AAB in a protected release job.
4. Verify its signer, package ID `org.bitcoin09.wallet`, version code, permissions and ARM64 libraries.
5. Install the Play-generated APK from an internal test track and rerun create, restore, lock, receive, scan, review, send and activity checks.

## Windows Store signing

Reserve the product name in Partner Center first, then copy the exact Partner Center identity name and Publisher value. They are public package identity values, not secrets.

Build the preflight MSIX with:

```powershell
./tools/release/package_windows_store.ps1 `
  -IdentityName '<Partner Center identity>' `
  -Publisher '<Partner Center publisher>' `
  -PublisherDisplayName '<verified company name>'
```

The script builds the wallet-only edition, maps version `0.1.33` to `1.33.0.0`, packages only the wallet shell, local wallet core and artwork, and unpacks the result again for validation. It deliberately does not sign. The Microsoft Store signs an accepted MSIX submission. A self-signed package is only for local testing and does not solve SmartScreen for public downloads.

After certification, install the app from its Store listing and confirm the publisher, launch, wallet creation, restore, backup, receive, payment review, activity, cleanup and uninstall flow.

## Apple signing

Do not create distribution certificates before the organization membership exists. Once approved:

1. Register `org.bitcoin09.wallet` to the organization team.
2. Create App Store distribution and Developer ID identities using the account holder's Apple team.
3. Put signing material in protected release secrets, not the repository.
4. Build and test the iPhone app through TestFlight.
5. Sign the Mac app with Developer ID, enable hardened runtime, submit it to Apple's notary service, staple the ticket and test Gatekeeper on a clean Mac.

## Store declarations

- Category: Finance
- Financial feature: Cryptocurrency wallet
- Custody: Non-custodial; the publisher cannot access or recover keys
- Exchange or purchase: None
- On-device mining: None in Android, iPhone or Microsoft Store editions
- Ads and analytics: None
- Account creation: None
- Camera: Optional, user-started recipient QR scanning on mobile
- Privacy URL: https://btc09.org/privacy.html
- Support URL: https://github.com/krutftw/bitcoin09

The wallet gateway processes public addresses in memory to return balances and activity. It receives a signed transaction only when a user broadcasts. Request bodies are not logged by the gateway. Cloudflare and the web server may retain IP address, route, time, status and browser details for reliability and abuse prevention. Confirm the live log fields and retention immediately before completing Google Data safety and Apple App Privacy forms.

## Release gate

Do not publish or announce the release until all of these are true:

- Organization publisher identity is verified on the target store.
- The final artifact is signed by the store or approved production identity.
- CI passes on Windows, macOS, Linux, Android and iPhone runners.
- The exact store-delivered build passes a clean-device wallet and recovery test.
- The final package contains no signing secrets, test wallets, recovery words or debug endpoints.
- Privacy, support, screenshots, description, age rating and financial declarations match the shipped build.
- Website and Discord are updated once, after the public listing or signed download is live and read back.
