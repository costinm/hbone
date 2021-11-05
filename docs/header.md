Special headers:

X-Forwarded-For

X-Forwarded-Client-Certificate

Google IAP:

X-Goog-Authenticated-User-Email: accounts.google.com:example@gmail.com
X-Goog-Authenticated-User-Id: accounts.google.com:userIDValue

Must validate: x-goog-iap-jwt-assertion: 
- ES256 + kid=Key from www.gstatic.com/iap/verify/public_key[-jwk]
- strips any x-goog-* header
- aud: /projects/PROJECT_NUMBER/apps/PROJECT_ID (appengine)
- aud: /projects/PROJECT_NUMBER/global/backendServices/SERVICE_ID
- iss: https://cloud.google.com/iap
- hd: hosted domain
- google: access level
