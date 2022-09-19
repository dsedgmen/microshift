package cryptomaterial

import (
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/authentication/user"

	"github.com/openshift/library-go/pkg/crypto"
)

type certificateChains struct {
	signers []*certificateSigner

	// fileBundles maps fileName -> signers, where fileName is the filename of a CA bundle
	// where PEM certificates should be stored
	fileBundles map[string][]string
}

type CertificateChains struct {
	signers map[string]*CertificateSigner
}

func NewCertificateChains(signers ...*certificateSigner) *certificateChains {
	return &certificateChains{
		signers: signers,

		fileBundles: make(map[string][]string),
	}
}

func (cs *certificateChains) WithSigners(signers ...*certificateSigner) *certificateChains {
	cs.signers = append(cs.signers, signers...)
	return cs
}

func (cs *certificateChains) WithCABundle(bundlePath string, signerNames ...string) *certificateChains {
	cs.fileBundles[bundlePath] = signerNames
	return cs
}

func (cs *certificateChains) Complete() (*CertificateChains, error) {
	completeChains := &CertificateChains{
		signers: make(map[string]*CertificateSigner),
	}

	for _, s := range cs.signers {
		s := s
		if _, ok := completeChains.signers[s.signerName]; ok {
			return nil, fmt.Errorf("signer name clash: %s", s.signerName)
		}

		completedSigner, err := s.Complete()
		if err != nil {
			return nil, fmt.Errorf("failed to complete signer %q: %w", s.signerName, err)
		}
		completeChains.signers[completedSigner.signerName] = completedSigner
	}

	bundlePreWrite := make(map[string][]byte, len(cs.fileBundles))
	for bundlePath, signers := range cs.fileBundles {
		for _, s := range signers {
			signerCACertPEM, err := completeChains.GetSigner(s).GetSignerCertPEM()
			if err != nil {
				return nil, fmt.Errorf("failed to retrieve cert PEM for signer %q: %w", s, err)
			}
			bundlePreWrite[bundlePath] = append(bundlePreWrite[bundlePath], append(signerCACertPEM, byte('\n'))...)
		}
	}

	for bundlePath, pemChain := range bundlePreWrite {
		if err := appendCertsToFile(bundlePath, pemChain); err != nil {
			return nil, err
		}
	}

	return completeChains, nil
}

func (cs *CertificateChains) GetSignerNames() []string {
	return certificateSignersMapKeysOrdered(cs.signers)
}

func (cs *CertificateChains) GetSigner(signerPath ...string) *CertificateSigner {
	if len(signerPath) == 0 {
		return nil
	}

	currentSigner := cs.signers[signerPath[0]]
	for _, fragment := range signerPath[1:] {
		if currentSigner != nil {
			currentSigner = currentSigner.GetSubCA(fragment)
		} else {
			return nil
		}
	}

	return currentSigner
}

func (cs *CertificateChains) GetCertKey(certPath ...string) ([]byte, []byte, error) {
	if len(certPath) == 0 {
		return nil, nil, fmt.Errorf("empty certificate path")
	}
	if len(certPath) == 1 {
		return nil, nil, fmt.Errorf("the CertificateChains struct only stores signers, the path must be at least 1 level deep")
	}

	signerPath := certPath[:len(certPath)-1]
	signer := cs.GetSigner(signerPath...)
	if signer == nil {
		return nil, nil, fmt.Errorf("no such signer in the path: %v", signerPath)
	}

	return signer.GetCertKey(certPath[len(certPath)-1])
}

type certificateSigner struct {
	signerName         string
	signerDir          string
	signerValidityDays int

	// signerConfig should only be used in case this is a sub-ca signer
	// It should be populated during CertificateSigner.SignSubCA()
	signerConfig             *crypto.CA
	subCAs                   []*certificateSigner
	clientCertificatesToSign []*ClientCertificateSigningRequestInfo
	serverCertificatesToSign []*ServingCertificateSigningRequestInfo
}

type CertificateSigner struct {
	signerName   string
	signerConfig *crypto.CA
	signerDir    string

	subCAs             map[string]*CertificateSigner
	signedCertificates map[string]*signedCertificateInfo
}

type ClientCertificateSigningRequestInfo struct {
	CertificateSigningRequestInfo

	UserInfo user.Info
}

type ServingCertificateSigningRequestInfo struct {
	CertificateSigningRequestInfo

	Hostnames []string
}

type CertificateSigningRequestInfo struct {
	Name         string
	ValidityDays int
}

type signedCertificateInfo struct {
	certDir   string
	tlsConfig *crypto.TLSCertificateConfig
}

// NewCertificateSigner returns a builder object for a certificate chain for the given signer
func NewCertificateSigner(signerName, signerDir string, validityDays int) *certificateSigner {
	return &certificateSigner{
		signerName:         signerName,
		signerDir:          signerDir,
		signerValidityDays: validityDays,
	}
}

func (s *certificateSigner) WithClientCertificates(signInfos ...*ClientCertificateSigningRequestInfo) *certificateSigner {
	s.clientCertificatesToSign = append(s.clientCertificatesToSign, signInfos...)
	return s
}

func (s *certificateSigner) WithServingCertificates(signInfos ...*ServingCertificateSigningRequestInfo) *certificateSigner {
	s.serverCertificatesToSign = append(s.serverCertificatesToSign, signInfos...)
	return s
}

func (s *certificateSigner) WithSubCAs(subCAsInfo ...*certificateSigner) *certificateSigner {
	s.subCAs = append(s.subCAs, subCAsInfo...)
	return s
}

func (s *certificateSigner) Complete() (*CertificateSigner, error) {
	// in case this is a sub-ca, it's already going to have the signer-config populated
	signerConfig := s.signerConfig
	if signerConfig == nil {
		var err error
		signerConfig, _, err = crypto.EnsureCA(
			CACertPath(s.signerDir),
			CAKeyPath(s.signerDir),
			CASerialsPath(s.signerDir),
			s.signerName,
			s.signerValidityDays,
		)

		if err != nil {
			return nil, fmt.Errorf("failed to generate %s CA certificate: %w", s.signerName, err)
		}
	}

	signerCompleted := &CertificateSigner{
		signerName:   s.signerName,
		signerConfig: signerConfig,
		signerDir:    s.signerDir,

		subCAs:             make(map[string]*CertificateSigner),
		signedCertificates: make(map[string]*signedCertificateInfo),
	}

	for _, subCA := range s.subCAs {
		subCA := subCA
		if err := signerCompleted.SignSubCA(subCA); err != nil {
			return nil, err
		}
	}

	for _, si := range s.clientCertificatesToSign {
		si := si
		if err := signerCompleted.SignClientCertificate(si); err != nil {
			return nil, err
		}
	}

	for _, si := range s.serverCertificatesToSign {
		si := si
		if err := signerCompleted.SignServingCertificate(si); err != nil {
			return nil, err
		}
	}

	return signerCompleted, nil
}

func (s *CertificateSigner) GetSignerCertPEM() ([]byte, error) {
	certPem, _, err := s.signerConfig.Config.GetPEMBytes()
	return certPem, err
}

func (s *CertificateSigner) SignSubCA(signerInfo *certificateSigner) error {
	subCAConfig, err := crypto.MakeCAConfigForDuration(
		signerInfo.signerName,
		time.Duration(signerInfo.signerValidityDays)*time.Hour*24,
		s.signerConfig,
	)
	if err != nil {
		return fmt.Errorf("failed to generate sub-CA %q: %w", signerInfo.signerName, err)
	}

	// lib-go automatically adds all the certs to the chain but KCM would not start
	// if there were more than 1 certs in the file
	subCAConfig.Certs = subCAConfig.Certs[:1]
	if err := subCAConfig.WriteCertConfigFile(
		CACertPath(signerInfo.signerDir),
		CAKeyPath(signerInfo.signerDir),
	); err != nil {
		return fmt.Errorf("failed to write the %q cert/key pair files: %w", signerInfo.signerName, err)
	}

	signerInfo.signerConfig = &crypto.CA{
		Config:          subCAConfig,
		SerialGenerator: &crypto.RandomSerialGenerator{},
	}

	subCertSigner, err := signerInfo.Complete()
	if err != nil {
		return err
	}

	s.subCAs[subCertSigner.signerName] = subCertSigner
	return nil
}

func (s *CertificateSigner) SignClientCertificate(signInfo *ClientCertificateSigningRequestInfo) error {
	certDir := filepath.Join(s.signerDir, signInfo.Name)

	tlsConfig, _, err := s.signerConfig.EnsureClientCertificate(
		ClientCertPath(certDir),
		ClientKeyPath(certDir),
		signInfo.UserInfo,
		signInfo.ValidityDays,
	)

	if err != nil {
		return fmt.Errorf("failed to generate client certificate for %q: %w", signInfo.Name, err)
	}

	s.signedCertificates[signInfo.Name] = &signedCertificateInfo{
		certDir:   certDir,
		tlsConfig: tlsConfig,
	}
	return nil
}

func (s *CertificateSigner) SignServingCertificate(signInfo *ServingCertificateSigningRequestInfo) error {
	certDir := filepath.Join(s.signerDir, signInfo.Name)

	tlsConfig, _, err := s.signerConfig.EnsureServerCert(
		ServingCertPath(certDir),
		ServingKeyPath(certDir),
		sets.NewString(signInfo.Hostnames...),
		signInfo.ValidityDays,
	)

	if err != nil {
		return fmt.Errorf("failed to generate serving certificate for %q: %w", signInfo.Name, err)
	}

	s.signedCertificates[signInfo.Name] = &signedCertificateInfo{
		certDir:   certDir,
		tlsConfig: tlsConfig,
	}
	return nil
}

func (s *CertificateSigner) GetCertNames() []string {
	return signedCertificateInfoMapKeysOrdered(s.signedCertificates)
}

func (s *CertificateSigner) GetCertKey(subjectName string) ([]byte, []byte, error) {
	certConfig, exists := s.signedCertificates[subjectName]
	if !exists {
		return nil, nil, fmt.Errorf("no certificate with name %q was found", subjectName)
	}

	return certConfig.tlsConfig.GetPEMBytes()
}

func (s *CertificateSigner) GetSubCANames() []string {
	return certificateSignersMapKeysOrdered(s.subCAs)
}

func (s *CertificateSigner) GetSubCA(signerName string) *CertificateSigner {
	return s.subCAs[signerName]
}

// TODO: merge the two below functions with generics once we've got go 1.18
func certificateSignersMapKeysOrdered(stringMap map[string]*CertificateSigner) []string {
	keys := make(sort.StringSlice, 0, len(stringMap))
	for k := range stringMap {
		keys = append(keys, k)
	}

	keys.Sort()
	return keys
}

func signedCertificateInfoMapKeysOrdered(stringMap map[string]*signedCertificateInfo) []string {
	keys := make(sort.StringSlice, 0, len(stringMap))
	for k := range stringMap {
		keys = append(keys, k)
	}

	keys.Sort()
	return keys
}