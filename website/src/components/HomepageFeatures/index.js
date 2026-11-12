import React from 'react';
import clsx from 'clsx';
import styles from './styles.module.css';

const FeatureList = [
  {
    title: 'Physical Infrastructure',
    Svg: require('@site/static/img/undraw_server_status.svg').default,
    description: (
      <>
        Powers physical machines on when cluster-autoscaler has work for them, and off when it has
        none: wake-on-LAN, IPMI, BMC, smart PDU, or your own script.
      </>
    ),
  },
  {
    title: 'Kubernetes Native',
    Svg: require('@site/static/img/undraw_kubernetes.svg').default,
    description: (
      <>
        Three resources: a pool's policy, a machine slot, one request for one machine.
        Admission refuses a second live claim on a slot, a reserved label key, and two slots
        claiming one node.
      </>
    ),
  },
  {
    title: 'Derived, Not Stored',
    Svg: require('@site/static/img/undraw_server_cluster.svg').default,
    description: (
      <>
        A machine is wanted when a live NodeClaim names its slot. The state machine's input is the
        claim, the observed power and the node's usability — no spec field carries an intent, so a
        GitOps apply cannot revert a scale-up.
      </>
    ),
  },
];

function Feature({Svg, title, description}) {
  return (
    <div className={clsx('col col--4')}>
      <div className="text--center">
        <Svg className={styles.featureSvg} role="img" />
      </div>
      <div className="text--center padding-horiz--md">
        <h3>{title}</h3>
        <p>{description}</p>
      </div>
    </div>
  );
}

export default function HomepageFeatures() {
  return (
    <section className={styles.features}>
      <div className="container">
        <div className="row">
          {FeatureList.map((props, idx) => (
            <Feature key={idx} {...props} />
          ))}
        </div>
      </div>
    </section>
  );
}